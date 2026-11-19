package nginx

// Streamed request bodies (owner: s-forward): on a streamed route the
// client's body is read once, straight into the upstream request, as nginx's
// `proxy_request_buffering off` reads it. Nothing is held, so the spill caps
// don't apply, but the body limit and client_body_timeout between reads do.
// A body that began going upstream can't be replayed, so it ends the
// request's retries (D7).
//
// Except one nginx buffers after all. nginx reads a body once, without
// waiting, before it connects upstream, and buffers a body that pass holds
// whole: ngx_http_read_client_request_body clears request_body_no_buffering
// for one pre-read whole with its head (ngx_http_request_body.c:159-163), and
// for one its first ngx_http_do_read_client_request_body reads whole
// (:208-213). That body goes with its length, and a failed attempt is
// retried, since nginx refuses a retry only while request_body_no_buffering
// is set (ngx_http_upstream_test_next, ngx_http_upstream.c:2817, and
// ngx_http_upstream_next, :4695). Go's server can't read without waiting, so
// the front sends the request head at once, and the body's first read
// decides: an h1 body of declared length that the read returns whole, as Go's
// server returns one it holds whole with the head, whatever the route's
// client_body_buffer_size, is kept in memory, reserved under the store's
// budget (D19), and a failed attempt replays it as a buffered body. The head
// is the same either way: its declared Content-Length and
// X-UR-Request-Buffering: off. Nothing is kept, as nginx keeps nothing, of:
//   - an h2 or h3 body: nginx runs an h2 request at its HEADERS frame
//     (ngx_http_v2_state_header_complete), before any of its body arrives;
//   - a body that waits for 100 Continue, which nginx's first pass reads
//     before the client sends any of it.
//
// And, unlike nginx:
//   - a chunked body streams, chunked, even when it came whole with its
//     head, since its upstream head would have to differ before Go's server
//     can tell;
//   - one read returns at most what the request's writer asks for,
//     ForwardSettings.CopyBufferBytes, and of a body that came with its head
//     only what Go's server read with the head, its 4 KiB buffer less the
//     head, so a longer declared body streams;
//   - a body that arrives whole only after its head may be kept where
//     nginx's first pass, already past, would stream it;
//   - a block that answers before the first read returns finds nothing
//     kept, though the read was about to return the body whole;
//   - a body the store has no room for streams unrefused.
//
// A read that fails on the client's side ends the request, a response being
// relayed included, as nginx ends it.

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
	// keeps a body the first read returns whole; nil keeps none
	keepBodyStore *bodyStore
	readCount     int
	readBytes     int64
	// the body the first read returned whole, kept for a retry
	keptBody        atomic.Pointer[storedBody]
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
// its length is unknown. keepBodyStore, when set, keeps a declared body that
// the first read returns whole.
func newForwardStreamedBody(ctx context.Context, body io.Reader, declaredLength int64, maxBytes int64, readTimeout time.Duration, setReadDeadline func(time.Time) error, keepBodyStore *bodyStore) *forwardStreamedBody {
	streamedBody := &forwardStreamedBody{
		ctx:             ctx,
		body:            body,
		declaredLength:  declaredLength,
		maxBytes:        maxBytes,
		readTimeout:     readTimeout,
		setReadDeadline: setReadDeadline,
		keepBodyStore:   keepBodyStore,
	}
	// an empty body is whole before any read
	streamedBody.whole.Store(declaredLength == 0)
	return streamedBody
}

// Marks that the upstream request began: its head is on the wire and the
// body is about to be read, so the request can't be retried unless the body
// is kept.
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

// The body the first read returned whole and the store kept, which a retry
// replays, or nil.
func (self *forwardStreamedBody) kept() *storedBody {
	return self.keptBody.Load()
}

// Releases the kept body, once the request is done with it: after every
// attempt's write has ended.
func (self *forwardStreamedBody) release() {
	if keptBody := self.kept(); keptBody != nil {
		keptBody.release()
	}
}

// Reads the client's body under its timeout and limit. A failure on the
// client's side is kept as the request's refusal; one caused by interrupt
// isn't. A first read that returns a declared body whole keeps it, when the
// body has a store to keep it in.
func (self *forwardStreamedBody) Read(p []byte) (int, error) {
	if self.interrupted.Load() {
		return 0, errBodyInterrupted
	}
	// the deadline this read waits under, zero when none
	var readDeadline time.Time
	if 0 < self.readTimeout && self.setReadDeadline != nil {
		readDeadline = time.Now().Add(self.readTimeout)
		self.setReadDeadline(readDeadline)
		// an interrupt whose past deadline this one may have replaced has
		// set its flag already: restore its deadline and stop
		if self.interrupted.Load() {
			self.setReadDeadline(time.Unix(1, 0))
			return 0, errBodyInterrupted
		}
	}
	firstRead := self.readCount == 0
	self.readCount += 1
	n, err := self.body.Read(p)
	if err == nil && !readDeadline.IsZero() {
		// the timeout bounds the wait for the client only, as nginx's read
		// timer runs only while it waits on the client; an h2 stream's
		// deadline would fail its body when it passed, a read pending or not,
		// while the request's writer waits on the block. An interrupt's past
		// deadline stays.
		self.setReadDeadline(time.Time{})
		if self.interrupted.Load() {
			self.setReadDeadline(time.Unix(1, 0))
		}
	}
	self.readBytes += int64(n)
	if 0 < self.maxBytes && self.maxBytes < self.readBytes {
		self.fail(&bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge})
		return n, io.ErrUnexpectedEOF
	}
	// a declared length counts as the end: the request's writer reads no
	// further than it, so it may never see the end itself
	if err == io.EOF || (err == nil && 0 <= self.declaredLength && self.declaredLength <= self.readBytes) {
		self.whole.Store(true)
		if firstRead && self.keepBodyStore != nil && 0 < self.declaredLength && self.readBytes == self.declaredLength {
			// a store over its budget or closed keeps nothing, and the body
			// streams on
			storedBody, openRefusal := self.keepBodyStore.open(self.declaredLength, true)
			if openRefusal == nil {
				if appendRefusal := storedBody.append(p[:n]); appendRefusal != nil {
					storedBody.release()
				} else {
					self.keptBody.Store(storedBody)
				}
			}
		}
	}
	if err == io.EOF {
		if !readDeadline.IsZero() {
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
