package nginx

// HTTP/1.1 connections to upstream blocks and their keepalive pools (owner:
// s-forward).
//
// The front speaks to blocks with its own small HTTP/1.1 client, built on
// net/http's message parsing, rather than with net/http's Transport, so that
// every attempt is one exchange it can count and classify (D7):
//   - the Transport redoes a request on its own after a reused connection
//     failed, even one the block may have read; here only a reused
//     connection that failed before any request byte was written is redone,
//     once, on a fresh connection to the same block, and the redo isn't an
//     attempt, as nginx's tries++ for a cached connection's error isn't;
//   - timeouts are native socket deadlines set before each read and each
//     write, as nginx's proxy_read_timeout and proxy_send_timeout bound the
//     wait between two operations, and they are per request; reads are
//     bounded from when the request was sent whole, or a response head
//     arrived, as nginx arms its read timer then;
//   - the response head is limited as nginx's proxy_buffer_size limits it,
//     every head of the attempt together, an informational one included.
//
// Pools follow nginx's upstream keepalive, kept per block for the process
// (nginx keeps `keepalive` idle connections per worker for a whole upstream,
// shared by its servers; the route file's value is used per block):
//   - at most RouteKeepalive.Connections idle connections, the most recently
//     used taken first; a full pool closes its least recently used to keep a
//     new one, as nginx's cache does;
//   - a connection serves at most RouteKeepalive.Requests requests, and is
//     kept only within RouteKeepalive.TimeSeconds of its dial
//     (keepalive_requests, keepalive_time); zero for either, or for
//     Connections, means no reuse;
//   - an idle connection closes after RouteKeepalive.TimeoutSeconds
//     (keepalive_timeout), by the group's sweep or when next taken; zero means
//     no reuse;
//   - a connection taken from the pool is first checked with a non-blocking
//     MSG_PEEK, as nginx's keepalive close handler checks one: a connection
//     the block closed, or that holds bytes nobody asked for, is dropped
//     before any request is written to it;
//   - dialed connections have SO_KEEPALIVE on with the system's probe
//     timing, nginx's `proxy_socket_keepalive on`.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The response head outgrew ForwardSettings.MaxUpstreamResponseHeaderBytes.
var errUpstreamHeadTooLarge = errors.New("upstream response head too large")

// A block's keepalive pool and the connections it has open. Safe for
// concurrent use.
type upstreamPool struct {
	group *upstreamGroup
	block *upstreamBlock

	stateLock sync.Mutex
	// idle connections, the least recently used first
	idleConns []*upstreamConnection
	// connections out of the pool, so close() can cut them
	activeConns map[*upstreamConnection]bool
	closed      bool
}

// One connection to a block. Its I/O state belongs to the exchange using it.
type upstreamConnection struct {
	pool   *upstreamPool
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer

	dialTime     time.Time
	requestCount int
	// when it was last returned to the pool
	idleTime time.Time

	// the current exchange's per-operation timeouts; non-positive means none
	readTimeout  time.Duration
	writeTimeout time.Duration
	// whether reads are bounded by readTimeout yet: from when the request
	// was written whole, or its response head read, as nginx arms
	// proxy_read_timeout only once the request is sent. Before that the
	// write's own timeout bounds a stalled block.
	readArmed atomic.Bool
	// head bytes the current exchange may still read; negative means no
	// limit
	headBytesLeft int64
	// the last error a read returned, end of stream included, so a failed
	// parse is told apart from a failed read
	readErr error
	// request bytes the socket accepted in the current exchange
	writtenBytes atomic.Int64
	closeOnce    sync.Once
}

func newUpstreamPool(group *upstreamGroup, block *upstreamBlock) *upstreamPool {
	return &upstreamPool{
		group:       group,
		block:       block,
		idleConns:   []*upstreamConnection{},
		activeConns: map[*upstreamConnection]bool{},
	}
}

// Whether the service's keepalive allows any reuse.
func (self *upstreamPool) keepaliveEnabled() bool {
	keepalive := self.group.service.Keepalive
	return 0 < keepalive.Connections && 0 < keepalive.Requests && 0 < keepalive.TimeSeconds && 0 < keepalive.TimeoutSeconds
}

// Whether an idle connection is past its keepalive timeout at now. Its
// keepalive time is checked when it is returned, as nginx checks it after
// each request, so an idle connection may serve one more request past it.
func (self *upstreamPool) expired(upstreamConn *upstreamConnection, now time.Time) bool {
	keepalive := self.group.service.Keepalive
	return time.Duration(keepalive.TimeoutSeconds)*time.Second <= now.Sub(upstreamConn.idleTime)
}

// A live idle connection, the most recently used first, or nil. Expired
// connections, and ones the block closed, are closed on the way.
func (self *upstreamPool) takeIdle() *upstreamConnection {
	now := time.Now()
	for {
		upstreamConn, expiredConns := func() (*upstreamConnection, []*upstreamConnection) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			expiredConns := []*upstreamConnection{}
			for 0 < len(self.idleConns) {
				n := len(self.idleConns) - 1
				upstreamConn := self.idleConns[n]
				self.idleConns[n] = nil
				self.idleConns = self.idleConns[:n]
				if self.expired(upstreamConn, now) {
					expiredConns = append(expiredConns, upstreamConn)
					continue
				}
				self.activeConns[upstreamConn] = true
				return upstreamConn, expiredConns
			}
			return nil, expiredConns
		}()
		for _, expiredConn := range expiredConns {
			expiredConn.close()
		}
		if upstreamConn == nil {
			return nil
		}
		if upstreamConnAlive(upstreamConn.conn) {
			return upstreamConn
		}
		self.forget(upstreamConn)
		upstreamConn.close()
	}
}

// Dials a new connection to the block. The error is the dial's.
func (self *upstreamPool) dial(ctx context.Context) (*upstreamConnection, error) {
	conn, err := self.group.dialContext(ctx, "tcp", self.block.target)
	if err != nil {
		return nil, err
	}
	upstreamConn := &upstreamConnection{
		pool:          self,
		conn:          conn,
		dialTime:      time.Now(),
		headBytesLeft: -1,
	}
	bufferBytes := self.group.settings.Forward.UpstreamBufferBytes
	if 0 < bufferBytes {
		upstreamConn.reader = bufio.NewReaderSize(upstreamConn, bufferBytes)
		upstreamConn.writer = bufio.NewWriterSize(upstreamConn, bufferBytes)
	} else {
		upstreamConn.reader = bufio.NewReader(upstreamConn)
		upstreamConn.writer = bufio.NewWriter(upstreamConn)
	}
	closed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.activeConns[upstreamConn] = true
		return false
	}()
	if closed {
		conn.Close()
		return nil, net.ErrClosed
	}
	return upstreamConn, nil
}

// Takes a connection back after an exchange: into the pool when reusable,
// the pool is open and the connection is within keepalive_requests and
// keepalive_time, else closed. A full pool closes its least recently used
// connection to keep this one.
func (self *upstreamPool) put(upstreamConn *upstreamConnection, reusable bool) {
	keepalive := self.group.service.Keepalive
	now := time.Now()
	reusable = reusable &&
		self.keepaliveEnabled() &&
		upstreamConn.requestCount < keepalive.Requests &&
		now.Sub(upstreamConn.dialTime) <= time.Duration(keepalive.TimeSeconds)*time.Second &&
		// bytes past the response are bytes nobody asked for
		upstreamConn.reader.Buffered() == 0
	evictedConn, kept := func() (*upstreamConnection, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.activeConns, upstreamConn)
		if !reusable || self.closed {
			return nil, false
		}
		var evictedConn *upstreamConnection
		if keepalive.Connections <= len(self.idleConns) {
			evictedConn = self.idleConns[0]
			self.idleConns = append(self.idleConns[:0], self.idleConns[1:]...)
		}
		upstreamConn.idleTime = now
		self.idleConns = append(self.idleConns, upstreamConn)
		return evictedConn, true
	}()
	if evictedConn != nil {
		evictedConn.close()
	}
	if !kept {
		upstreamConn.close()
	}
}

// Drops a connection out of the pool's tracking, without closing it.
func (self *upstreamPool) forget(upstreamConn *upstreamConnection) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.activeConns, upstreamConn)
}

// Closes idle connections past their keepalive timeout.
func (self *upstreamPool) sweep(now time.Time) {
	expiredConns := func() []*upstreamConnection {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		expiredConns := []*upstreamConnection{}
		keptConns := self.idleConns[:0]
		for _, upstreamConn := range self.idleConns {
			if self.expired(upstreamConn, now) {
				expiredConns = append(expiredConns, upstreamConn)
			} else {
				keptConns = append(keptConns, upstreamConn)
			}
		}
		clear(self.idleConns[len(keptConns):])
		self.idleConns = keptConns
		return expiredConns
	}()
	for _, upstreamConn := range expiredConns {
		upstreamConn.close()
	}
}

// Closes every connection, idle or in use, and keeps no more.
func (self *upstreamPool) close() {
	upstreamConns := func() []*upstreamConnection {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		upstreamConns := append([]*upstreamConnection{}, self.idleConns...)
		for upstreamConn := range self.activeConns {
			upstreamConns = append(upstreamConns, upstreamConn)
		}
		self.idleConns = nil
		clear(self.activeConns)
		return upstreamConns
	}()
	for _, upstreamConn := range upstreamConns {
		upstreamConn.close()
	}
}

// The number of idle connections.
func (self *upstreamPool) idleCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.idleConns)
}

// Whether an idle connection is still usable: a non-blocking MSG_PEEK finds
// nothing to read. End of stream, an error, or bytes the block sent unasked
// mean it isn't (ngx_http_upstream_keepalive_close_handler). A connection
// without a raw descriptor is assumed alive.
func upstreamConnAlive(conn net.Conn) bool {
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		return true
	}
	rawConn, err := syscallConn.SyscallConn()
	if err != nil {
		return false
	}
	alive := false
	var peekBuffer [1]byte
	err = rawConn.Read(func(fd uintptr) bool {
		// nothing to read is the only live answer; zero bytes is the end of
		// the stream, and any byte is one nobody asked for
		_, _, peekErr := syscall.Recvfrom(int(fd), peekBuffer[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		alive = peekErr == syscall.EAGAIN || peekErr == syscall.EWOULDBLOCK
		// never wait for readiness: one non-blocking look decides
		return true
	})
	return err == nil && alive
}

// Reads from the block under the exchange's read timeout and head limit.
func (self *upstreamConnection) Read(p []byte) (int, error) {
	if 0 <= self.headBytesLeft {
		if self.headBytesLeft == 0 {
			self.readErr = errUpstreamHeadTooLarge
			return 0, errUpstreamHeadTooLarge
		}
		if self.headBytesLeft < int64(len(p)) {
			p = p[:self.headBytesLeft]
		}
	}
	if 0 < self.readTimeout && self.readArmed.Load() {
		self.conn.SetReadDeadline(time.Now().Add(self.readTimeout))
	}
	n, err := self.conn.Read(p)
	if 0 <= self.headBytesLeft {
		self.headBytesLeft -= int64(n)
	}
	if err != nil {
		self.readErr = err
	}
	return n, err
}

// Starts bounding reads by the read timeout, a pending read included. A
// read that saw the connection unarmed has no deadline, so arming sets one
// under it; a read after arming sets its own.
func (self *upstreamConnection) armRead() {
	if self.readArmed.Swap(true) {
		return
	}
	if 0 < self.readTimeout {
		self.conn.SetReadDeadline(time.Now().Add(self.readTimeout))
	}
}

// Writes to the block under the exchange's send timeout, counting what the
// socket accepted.
func (self *upstreamConnection) Write(p []byte) (int, error) {
	if 0 < self.writeTimeout {
		self.conn.SetWriteDeadline(time.Now().Add(self.writeTimeout))
	}
	n, err := self.conn.Write(p)
	self.writtenBytes.Add(int64(n))
	return n, err
}

// Closes the socket. Safe to call more than once and concurrently.
func (self *upstreamConnection) close() {
	self.closeOnce.Do(func() {
		self.conn.Close()
	})
}

// How an attempt failed, in nginx's proxy_next_upstream terms.
type upstreamFailure int

const (
	upstreamFailureNone upstreamFailure = iota
	// nginx's `error`: a refused or failed dial, a failed write, or a read
	// that failed or found the connection closed before the response head.
	// Retried; the final answer is 502.
	upstreamFailureError
	// nginx's `timeout`: the dial, a write or a read past its timeout before
	// the response head. Retried; the final answer is 504.
	upstreamFailureTimeout
	// nginx's `invalid_header`: a malformed response head, or one over the
	// limit, or a 101 to a request that asked for no upgrade. Never
	// retried; the answer is 502.
	upstreamFailureInvalidHeader
	// the client went away, or the request body failed on the client's side
	upstreamFailureClient
)

// The attempt result label for a failure.
func (self upstreamFailure) String() string {
	switch self {
	case upstreamFailureError:
		return "error"
	case upstreamFailureTimeout:
		return "timeout"
	case upstreamFailureInvalidHeader:
		return "invalid_header"
	case upstreamFailureClient:
		return "client"
	default:
		return "none"
	}
}

// Classifies an i/o error: a deadline is a timeout, anything else an error.
func upstreamIoFailure(err error) upstreamFailure {
	var netErr net.Error
	if errors.Is(err, errUpstreamHeadTooLarge) {
		return upstreamFailureInvalidHeader
	}
	if errors.As(err, &netErr) && netErr.Timeout() {
		return upstreamFailureTimeout
	}
	return upstreamFailureError
}

// One request as the upstream sees it, the same for every attempt.
type upstreamRequest struct {
	// the request line and headers
	head []byte
	// the method, for parsing the response (a HEAD's has no body)
	method string
	// the framing the head declares
	bodyFraming forwardBodyFraming
	// a buffered body, replayed by each attempt, or nil
	storedBody *storedBody
	// a streamed body, read once, or nil
	streamedBody *forwardStreamedBody
	// the request asks for an upgrade
	upgrade      bool
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// One request and its response on one connection. The request write runs
// in its own goroutine, so a response that starts before the body is sent is
// read at once, as nginx reads it, and a streamed body keeps flowing while
// the response does. Every path ends in finish, which joins that goroutine,
// so no body is read after its exchange ends.
type upstreamExchange struct {
	conn     *upstreamConnection
	request  *upstreamRequest
	response *http.Response
	// the write goroutine's result, sent once
	writeResult chan error
	writeErr    error
	writeJoined bool
	finished    bool
	// set once the response head is read, so a failed write no longer cuts
	// the connection under a response being relayed
	headRead atomic.Bool
	// set when the write failed first and cut the connection, so the read
	// that then fails is classified by the write's error
	writeCut atomic.Bool
	// set by finish before it cuts the connection itself, so the write that
	// then fails isn't taken for the cause
	finishing atomic.Bool
	// set when the request was written whole, just before the result is sent
	writeDone atomic.Bool
	// stops the cancellation hook; false when it already ran
	stopCancel func() bool

	// gates a streamed body: once stopped, the write may not begin reading
	// it, and a body already begun is interrupted
	bodyStateLock sync.Mutex
	bodyBegan     bool
	bodyStopped   bool
}

// Writes the request and reads the response head. On a failure the
// connection is closed and the exchange already finished; nothingWritten
// reports that no request byte reached the socket, so the block can't have
// seen the request.
func (self *upstreamGroup) exchange(ctx context.Context, upstreamConn *upstreamConnection, request *upstreamRequest) (exchange *upstreamExchange, failure upstreamFailure, nothingWritten bool) {
	upstreamConn.requestCount += 1
	upstreamConn.readTimeout = request.readTimeout
	upstreamConn.writeTimeout = request.writeTimeout
	upstreamConn.readErr = nil
	upstreamConn.writtenBytes.Store(0)
	upstreamConn.readArmed.Store(false)
	// a pooled connection keeps its last exchange's deadlines
	upstreamConn.conn.SetDeadline(time.Time{})
	if maxHeadBytes := self.settings.Forward.MaxUpstreamResponseHeaderBytes; 0 < maxHeadBytes {
		upstreamConn.headBytesLeft = int64(maxHeadBytes)
	} else {
		upstreamConn.headBytesLeft = -1
	}

	exchange = &upstreamExchange{
		conn:        upstreamConn,
		request:     request,
		writeResult: make(chan error, 1),
		// a canceled request cuts its connection, which ends any read or
		// write on it; a cut connection is never reused
		stopCancel: context.AfterFunc(ctx, upstreamConn.close),
	}
	go func() {
		err := exchange.writeRequest()
		if err == nil {
			exchange.writeDone.Store(true)
			upstreamConn.armRead()
		} else if !exchange.headRead.Load() && !exchange.finishing.Load() {
			// the block can't answer a request it didn't get whole, so end
			// the wait for its response head
			exchange.writeCut.Store(true)
			upstreamConn.close()
		}
		exchange.writeResult <- err
	}()

	response, err := readUpstreamResponseHead(upstreamConn.reader, request.method)
	if err == nil && response.StatusCode == http.StatusSwitchingProtocols && !request.upgrade {
		err = errors.New("upstream switched protocols unasked")
		upstreamConn.readErr = nil
	}
	if err != nil {
		exchange.finish(false)
		if ctx.Err() != nil {
			return nil, upstreamFailureClient, false
		}
		if request.streamedBody != nil && request.streamedBody.failed() {
			return nil, upstreamFailureClient, false
		}
		nothingWritten = upstreamConn.writtenBytes.Load() == 0
		switch {
		case exchange.writeCut.Load() && exchange.writeErr != nil:
			// the write failed first and cut the connection under the read
			return nil, upstreamIoFailure(exchange.writeErr), nothingWritten
		case upstreamConn.readErr != nil:
			// the read failed, or found the connection closed
			return nil, upstreamIoFailure(upstreamConn.readErr), nothingWritten
		default:
			// the bytes read don't parse as a response head
			return nil, upstreamFailureInvalidHeader, nothingWritten
		}
	}
	upstreamConn.headBytesLeft = -1
	exchange.headRead.Store(true)
	// the body's reads are bounded whether or not the request is still
	// streaming up, as nginx bounds a response it relays
	upstreamConn.armRead()
	exchange.response = response
	return exchange, upstreamFailureNone, false
}

// Reads the final response head, past informational ones, as nginx skips
// them. A 101 is final. The bytes after the head stay in reader.
func readUpstreamResponseHead(reader *bufio.Reader, method string) (*http.Response, error) {
	request := &http.Request{
		Method: method,
	}
	for {
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusSwitchingProtocols {
			return response, nil
		}
		if http.StatusContinue <= response.StatusCode && response.StatusCode < http.StatusOK {
			continue
		}
		return response, nil
	}
}

// Writes the request head and body to the connection and flushes it. A
// streamed body's head is flushed before its first byte is read, so a
// connection that fails that early never consumed any of the body.
func (self *upstreamExchange) writeRequest() error {
	request := self.request
	writer := self.conn.writer
	if _, err := writer.Write(request.head); err != nil {
		return err
	}
	var body io.Reader
	switch {
	case request.storedBody != nil:
		body = request.storedBody.newReader()
	case request.streamedBody != nil:
		if err := writer.Flush(); err != nil {
			return err
		}
		if !self.beginBody() {
			return errBodyInterrupted
		}
		body = request.streamedBody
	}
	switch {
	case body == nil:
	case request.bodyFraming.known:
		n, err := io.Copy(writer, io.LimitReader(body, request.bodyFraming.length))
		if err != nil {
			return err
		}
		if n < request.bodyFraming.length {
			return io.ErrUnexpectedEOF
		}
	case request.bodyFraming.chunked:
		chunkedWriter := httputil.NewChunkedWriter(writer)
		if _, err := io.Copy(chunkedWriter, body); err != nil {
			return err
		}
		if err := chunkedWriter.Close(); err != nil {
			return err
		}
		if _, err := writer.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// Lets the write begin reading a streamed body, unless the exchange already
// stopped it; a body begun can no longer be retried.
func (self *upstreamExchange) beginBody() bool {
	self.bodyStateLock.Lock()
	defer self.bodyStateLock.Unlock()
	if self.bodyStopped {
		return false
	}
	self.bodyBegan = true
	self.request.streamedBody.begin()
	return true
}

// Stops a streamed body for this exchange: the write may no longer begin
// it, and one it began is interrupted. A body never begun stays usable for a
// retry.
func (self *upstreamExchange) stopBody() {
	self.bodyStateLock.Lock()
	defer self.bodyStateLock.Unlock()
	self.bodyStopped = true
	if self.bodyBegan {
		self.request.streamedBody.interrupt()
	}
}

// Waits for the request write to end on its own, as a tunnel does before it
// takes the connection over, and returns its error. The connection stays
// out of the pool, and finish still decides its fate.
func (self *upstreamExchange) joinWrite() error {
	if !self.writeJoined {
		self.writeErr = <-self.writeResult
		self.writeJoined = true
	}
	return self.writeErr
}

// Ends the exchange: unblocks and joins the request write, then puts the
// connection back in its pool when reusable (the response was read to its
// end and allows reuse), and the request was written whole and nothing cut
// the connection; otherwise closes it. Safe to call more than once; only the
// first call decides.
func (self *upstreamExchange) finish(reusable bool) {
	if self.finished {
		return
	}
	self.finished = true
	if !self.writeJoined {
		if !self.writeDone.Load() {
			// the response ended, or the attempt failed, before the request
			// was written whole: the block won't read the rest, so cut the
			// connection and stop the body
			reusable = false
			self.finishing.Store(true)
			self.conn.close()
			if self.request.streamedBody != nil {
				self.stopBody()
			}
		}
		self.writeErr = <-self.writeResult
		self.writeJoined = true
	}
	if !self.stopCancel() {
		// the request was canceled and the connection cut
		reusable = false
	}
	if self.writeErr != nil {
		reusable = false
	}
	self.conn.pool.put(self.conn, reusable)
}
