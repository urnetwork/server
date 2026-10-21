package router

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// The load balancer marks a request whose body it streams through to the
// service (a warp `streamable` service or a `streamable_paths` match) with
// this header, and strips any client-supplied value on every buffered route.
// A buffered body arrives whole at lan speed from the lb; a streamed body
// arrives at the pace of the client, which is what StreamingBody adjusts for.
const RequestBufferingHeader = "X-UR-Request-Buffering"
const RequestBufferingOff = "off"

// The lb stops sending the body once it has the response headers, so what is
// left to drain is at most its in-flight buffer plus what the kernel holds. A
// direct peer that keeps writing past this is cut instead of drained.
const streamingDrainMaxBytes = 1 << 20

// nginx's own lingering_timeout
const defaultStreamingDrainTimeout = 5 * time.Second

// moving a deadline is a syscall; a body that is making progress does not
// need it moved on every read
const streamingArmInterval = time.Second

// StreamingBody is the body policy for a request whose body streams from the
// client instead of arriving whole from the lb.
//
// net/http arms `ReadTimeout` before the headers, covering the entire body,
// and arms `WriteTimeout` the moment the headers are parsed, before the body.
// Both assume the body is already in hand. Streamed, `ReadTimeout` becomes a
// cap on the client's upload and the upload eats into the `WriteTimeout`
// response budget, so the router replaces them per request with these.
//
// A handler that responds before consuming the body (an early 401 or 413)
// must not let the connection close with unread bytes: that sends a tcp
// reset, which can discard the response still in flight to the lb (nginx
// tickets #1037, #2457) and surface as a 502. The router drains the rest of
// the body after such a response instead (lingering close, RFC 7230 §6.6).
type StreamingBody struct {
	// the longest pause between two successive body reads, re-armed as the
	// body makes progress. Zero leaves the server read deadline in place.
	IdleTimeout time.Duration
	// a cap on the whole body transfer. Zero is no cap beyond IdleTimeout.
	TransferTimeout time.Duration
	// the response deadline armed once the body reaches EOF, replacing the
	// server `WriteTimeout` that began at the end of the headers. Zero
	// leaves the server write deadline in place.
	ResponseTimeout time.Duration
	// how long to keep reading a body the handler left unread after it
	// responded. The lb closes its side as soon as it has relayed the
	// response, so this normally ends at once; it bounds a direct peer that
	// keeps sending. Zero is the nginx lingering_timeout default of 5s.
	DrainTimeout time.Duration
}

func (self StreamingBody) drainTimeout() time.Duration {
	if 0 < self.DrainTimeout {
		return self.DrainTimeout
	}
	return defaultStreamingDrainTimeout
}

// streamingPolicy is the StreamingBody for a request, or false when the body
// is not streamed. A route's own policy applies regardless of the lb (the
// alt front serves the same routes over http3 with no lb at all); otherwise
// the lb's header selects the router default. A request with no body has
// nothing to stream: the lb never streams one either.
func (self *Router) streamingPolicy(route *Route, r *http.Request) (StreamingBody, bool) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return StreamingBody{}, false
	}
	if route.streaming != nil {
		return *route.streaming, true
	}
	if strings.EqualFold(r.Header.Get(RequestBufferingHeader), RequestBufferingOff) {
		return self.streaming, true
	}
	return StreamingBody{}, false
}

// streamingBody owns the connection deadlines while a streamed body is read.
type streamingBody struct {
	io.ReadCloser
	controller *http.ResponseController
	policy     StreamingBody
	start      time.Time
	armedAt    time.Time
	eof        bool
	// the writer cannot set deadlines (a test recorder): stop asking
	unsupported bool
}

func newStreamingBody(body io.ReadCloser, w http.ResponseWriter, policy StreamingBody) *streamingBody {
	self := &streamingBody{
		ReadCloser: body,
		controller: http.NewResponseController(w),
		policy:     policy,
		start:      time.Now(),
	}
	// the server read deadline was armed before the headers; move it out now
	self.arm(self.start, true)
	return self
}

// arm moves the read deadline to the end of the next idle window, capped by
// the transfer deadline.
func (self *streamingBody) arm(now time.Time, force bool) {
	if self.unsupported {
		return
	}
	if !force && now.Sub(self.armedAt) < streamingArmInterval {
		return
	}
	var deadline time.Time
	if 0 < self.policy.IdleTimeout {
		deadline = now.Add(self.policy.IdleTimeout)
	}
	if 0 < self.policy.TransferTimeout {
		transferDeadline := self.start.Add(self.policy.TransferTimeout)
		if deadline.IsZero() || transferDeadline.Before(deadline) {
			deadline = transferDeadline
		}
	}
	if deadline.IsZero() {
		return
	}
	if err := self.controller.SetReadDeadline(deadline); err != nil {
		self.unsupported = true
		return
	}
	self.armedAt = now
}

// `io.Reader`
func (self *streamingBody) Read(b []byte) (int, error) {
	n, err := self.ReadCloser.Read(b)
	if self.eof {
		return n, err
	}
	if 0 < n {
		self.arm(time.Now(), false)
	}
	if err == io.EOF {
		self.eof = true
		self.complete()
	}
	return n, err
}

// the body is in hand: the response budget starts now, not at the headers
func (self *streamingBody) complete() {
	if 0 < self.policy.ResponseTimeout {
		// unsupported by the writer is the same no-op as for the read deadline
		self.controller.SetWriteDeadline(time.Now().Add(self.policy.ResponseTimeout))
	}
}

// drain reads out whatever the handler left of the body, after its response.
// The response goes out first: the lb stops sending the body when it sees the
// response headers and then closes, which is what normally ends the drain.
func (self *streamingBody) drain() {
	if self.eof {
		return
	}
	self.eof = true
	self.controller.Flush()
	if !self.unsupported {
		self.controller.SetReadDeadline(time.Now().Add(self.policy.drainTimeout()))
	}
	io.CopyN(io.Discard, self.ReadCloser, streamingDrainMaxBytes)
}
