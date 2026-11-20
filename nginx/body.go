package nginx

// Request body storage for buffered forwarding (owner: s-forward; D7, D19,
// D21). nginx reads a buffered body whole before it picks an upstream, so a
// retry can send it again; the front does the same:
//   - a body keeps its first bytes in memory: min(its declared length, its
//     route's client_body_buffer_size or BodySettings.MemoryPrefixBytes), the
//     whole prefix for a body of unknown length, reserved from the process's
//     BodySettings.MemoryBudgetBytes before the first read (D7, D19). With no
//     room, the front answers a local 503 before reading anything. An h3
//     request without a content-length reserves once its first bytes arrive
//     instead, and a 503 comes after that read: the vendored http3 gives
//     every such request a body of unknown length, a GET's included, and one
//     whose stream ends first has no body, so it holds nothing
//     (bodyReadOptions.absentWhenEmpty);
//   - the bytes past the prefix go to one spill file in BodySettings.SpillDir,
//     taken under BodySettings.SpillMaxFiles when the first such byte
//     arrives, and reserved under BodySettings.SpillMaxBytes as bytes arrive
//     (D21). Past either cap, or when the file can't be created or written,
//     a local 503;
//   - a declared length over BodySettings.MaxBodyBytes is a 413 before
//     reading; a body of unknown length that crosses it is a 413 at that
//     point;
//   - a read that waits longer than client_body_timeout is nginx's
//     NGX_HTTP_REQUEST_TIME_OUT, which ends the request with no response
//     (upstreamGroup.refuse);
//   - a client that goes away ends the request with no response too. On h1
//     that includes one that ends its body early, since Go's server reports
//     the closed side by canceling the request, as nginx terminates a
//     request whose connection closed under its body. A client that resets
//     its connection, its h2 stream or its h3 request stream mid-body has
//     gone away too, by the read's error, which can come before the
//     request's context ends or, on h3, without it (bodyReadClientGone).
//     Elsewhere a body that ends early, on h2 or h3, or an h1 chunked body
//     that breaks its framing, is a 400;
//   - a stored body replays byte for byte, memory then file, once per
//     attempt.
//
// A local 503 is counted apart from upstream 503s (the group's refusal
// counter) and is never retried, since no upstream was tried.
//
// Every exit releases what a body holds: its reserved bytes, its file's
// descriptor, and the file itself. A spill file is unlinked as soon as it is
// created, so it never outlives its descriptor, even if the process dies;
// its disk space is freed when the descriptor closes. Release runs from a
// defer, so it runs on success, on any error, on cancellation and on a
// panic.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// Refusal reasons, the refusal counter's label values. A closed set.
const (
	// 413: the body is over BodySettings.MaxBodyBytes
	bodyRefusalTooLarge = "too_large"
	// local 503: no room in BodySettings.MemoryBudgetBytes for the prefix
	bodyRefusalMemoryBudget = "memory_budget"
	// local 503: no room under BodySettings.SpillMaxBytes
	bodyRefusalSpillBytes = "spill_bytes"
	// local 503: no file left under BodySettings.SpillMaxFiles
	bodyRefusalSpillFiles = "spill_files"
	// local 503: the spill file failed to create or write
	bodyRefusalSpillError = "spill_error"
	// local 503: the store is closed
	bodyRefusalClosed = "closed"
	// a read waited past client_body_timeout, nginx's 408, which it answers
	// with no response, as the front does but on h3
	bodyRefusalTimeout = "timeout"
	// 400: the client ended its body early or sent a malformed one
	bodyRefusalIncomplete = "incomplete"
	// nothing answered: the client went away
	bodyRefusalCanceled = "canceled"
)

// A body read was stopped by the forwarder itself, after the upstream's
// response ended.
var errBodyInterrupted = errors.New("request body interrupted")

// A stored body was released while a reader still used it.
var errBodyReleased = errors.New("request body released")

// Why the front refused a request body, and what it answers.
type bodyRefusal struct {
	// the status the front answers; 0 when the client went away, so there
	// is no one to answer
	status int
	// the refusal counter's label
	reason string
}

// A spill file: written once, then read at any offset. *os.File is one;
// tests substitute their own to inject failures.
type bodySpillFile interface {
	io.Writer
	io.ReaderAt
	io.Closer
}

// The process's body storage, one per front and shared by all its services.
// Safe for concurrent use.
type bodyStore struct {
	settings *BodySettings
	// creates a spill file; tests replace it before use
	createSpillFile func() (bodySpillFile, error)
	readBuffers     sync.Pool

	stateLock   sync.Mutex
	memoryBytes int64
	spillBytes  int64
	spillFiles  int
	// every body not yet released, so close can release them
	bodies map[*storedBody]bool
	closed bool
}

// Builds an empty store under settings' budget and caps.
func newBodyStore(settings *BodySettings) *bodyStore {
	store := &bodyStore{
		settings: settings,
		bodies:   map[*storedBody]bool{},
	}
	store.createSpillFile = store.createTempSpillFile
	store.readBuffers.New = func() any {
		buffer := make([]byte, max(settings.ReadBufferBytes, 1))
		return &buffer
	}
	return store
}

// Creates a spill file in SpillDir, or os.TempDir() when it is empty, and
// unlinks it at once.
func (self *bodyStore) createTempSpillFile() (bodySpillFile, error) {
	file, err := os.CreateTemp(self.settings.SpillDir, "lbalt-body-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// The client side of one body read.
type bodyReadOptions struct {
	// the length the client declared, or -1 when unknown
	declaredLength int64
	// the bytes held in memory before the rest spills; non-positive holds
	// none
	prefixBytes int64
	// the longest wait between two reads; non-positive means no bound
	readTimeout time.Duration
	// sets the client's read deadline, as a ResponseController does. Nil, or
	// one that fails, leaves reads bounded only by the request's context.
	setReadDeadline func(time.Time) error
	// a body of unknown length that ends before its first byte is no body:
	// its memory prefix is reserved only once a read returns bytes, and read
	// returns a nil body for one that ends first. For an h3 request without
	// a content-length: the vendored http3 gives each a body of unknown
	// length, a GET's included, and reports a stream that ended with its
	// head only to a read. A declared length ignores it.
	absentWhenEmpty bool
}

// Reads a whole body into the store, under the rules in the file comment.
// On a refusal nothing stays reserved or open, and a body that
// absentWhenEmpty finds absent is a nil body with no refusal. The client's
// read deadline is cleared when the body has been read whole, so nothing
// times out the connection while the upstream answers; after a failed read
// it stays, so the server closes the connection rather than wait on the
// client.
func (self *bodyStore) read(ctx context.Context, body io.Reader, options bodyReadOptions) (*storedBody, *bodyRefusal) {
	maxBodyBytes := self.settings.MaxBodyBytes
	if 0 < maxBodyBytes && maxBodyBytes < options.declaredLength {
		return nil, &bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge}
	}
	memoryReservedBytes := max(options.prefixBytes, 0)
	if 0 <= options.declaredLength {
		memoryReservedBytes = min(memoryReservedBytes, options.declaredLength)
	}
	var storedBody *storedBody
	stored := false
	defer func() {
		if storedBody != nil && !stored {
			storedBody.release()
		}
	}()
	// registers the body with its prefix reserved: before the first read,
	// or for a body that may be absent, before its first bytes are stored
	open := func() *bodyRefusal {
		var refusal *bodyRefusal
		storedBody, refusal = self.open(memoryReservedBytes, 0 <= options.declaredLength)
		return refusal
	}
	if !options.absentWhenEmpty || 0 <= options.declaredLength {
		if refusal := open(); refusal != nil {
			return nil, refusal
		}
	}

	readBuffer := self.readBuffers.Get().(*[]byte)
	defer self.readBuffers.Put(readBuffer)
	timedRead := 0 < options.readTimeout && options.setReadDeadline != nil
	for {
		if ctx.Err() != nil {
			return nil, &bodyRefusal{status: 0, reason: bodyRefusalCanceled}
		}
		if timedRead {
			options.setReadDeadline(time.Now().Add(options.readTimeout))
		}
		n, err := body.Read(*readBuffer)
		if timedRead && (err == nil || err == io.EOF) {
			// An h2 deadline can fail the stream between reads while the
			// store waits on a spill file.
			options.setReadDeadline(time.Time{})
		}
		if 0 < n {
			if storedBody == nil {
				if refusal := open(); refusal != nil {
					return nil, refusal
				}
			}
			if refusal := storedBody.append((*readBuffer)[:n]); refusal != nil {
				return nil, refusal
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// the deadline stays: the server must not wait on this client
			// to discard what remains of its body
			return nil, bodyReadRefusal(ctx, err)
		}
	}
	if 0 <= options.declaredLength && storedBody.length() != options.declaredLength {
		// a body that ended short of its declared length, which Go's h1 and
		// h2 servers report as a failed read, but the vendored http3 as the
		// stream's end: RFC 9114 §4.1.2 makes such a request malformed, and
		// nginx answers it 400 ("client prematurely closed stream")
		return nil, &bodyRefusal{status: http.StatusBadRequest, reason: bodyRefusalIncomplete}
	}
	stored = true
	return storedBody, nil
}

// The refusal for a failed client body read: its read timed out (408), the
// client went away, or its body ended early or broke its framing (400). The
// timeout is checked first: Go's HTTP/1.1 server cancels the request's
// context on any failed read, a timed out one included. The client went away
// when the request's context has ended, or when the read's own error says so
// (bodyReadClientGone), which can come before the context ends, or without
// it.
func bodyReadRefusal(ctx context.Context, err error) *bodyRefusal {
	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &bodyRefusal{status: http.StatusRequestTimeout, reason: bodyRefusalTimeout}
	}
	if ctx.Err() != nil || bodyReadClientGone(err) {
		return &bodyRefusal{status: 0, reason: bodyRefusalCanceled}
	}
	return &bodyRefusal{status: http.StatusBadRequest, reason: bodyRefusalIncomplete}
}

// The text of the error Go's h2 server fails each open stream's body with
// once its connection has ended, before it cancels the stream's context
// (net/http/internal/http2's errClientDisconnected). The error isn't
// exported, so it is matched by its text, which
// TestBodyReadClientGoneOfRealClientResets pins with Go's own h2 server.
const bodyHttp2ClientDisconnectedText = "client disconnected"

// Whether a failed body read's error says the client reset or lost its
// request: its connection, or its h2 or h3 stream. Each of these can fail
// the read before the request's context ends:
//   - a TCP reset (ECONNRESET);
//   - an h2 stream the client reset with CANCEL, the code a client abandons
//     a request with and Go's h2 server never sends. That server cancels the
//     request's context before it fails the body for a client's reset of any
//     code, so the other codes count by the context; for its own resets, for
//     a client's protocol error, it cancels the context just after, and the
//     error leaves those to the context too;
//   - an h2 connection that ended (bodyHttp2ClientDisconnectedText): Go's
//     server fails the body first;
//   - an h3 request stream the client reset, or a QUIC connection it closed
//     or reset: the vendored http3 fails the body with a remote http3.Error,
//     or quic's own error, and cancels the request's context only once the
//     response's side is stopped too, so a client that resets only its
//     request's side never ends it. A local error is the front's own doing.
func bodyReadClientGone(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) || err.Error() == bodyHttp2ClientDisconnectedText {
		return true
	}
	var http2StreamErr http2.StreamError
	if errors.As(err, &http2StreamErr) {
		return http2StreamErr.Code == http2.ErrCodeCancel
	}
	var http3Err *http3.Error
	if errors.As(err, &http3Err) {
		return http3Err.Remote
	}
	var quicStreamErr *quic.StreamError
	if errors.As(err, &quicStreamErr) {
		return quicStreamErr.Remote
	}
	var quicApplicationErr *quic.ApplicationError
	if errors.As(err, &quicApplicationErr) {
		return quicApplicationErr.Remote
	}
	var quicTransportErr *quic.TransportError
	if errors.As(err, &quicTransportErr) {
		return quicTransportErr.Remote
	}
	var quicStatelessResetErr *quic.StatelessResetError
	return errors.As(err, &quicStatelessResetErr)
}

// Registers a new body with its memory prefix reserved, or refuses with a
// local 503. The first append allocates a known length's reserved prefix.
func (self *bodyStore) open(memoryReservedBytes int64, knownLength bool) (*storedBody, *bodyRefusal) {
	storedBody, refusal := func() (*storedBody, *bodyRefusal) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil, &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalClosed}
		}
		memoryBudgetBytes := self.settings.MemoryBudgetBytes
		if 0 < memoryBudgetBytes && memoryBudgetBytes-self.memoryBytes < memoryReservedBytes {
			return nil, &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalMemoryBudget}
		}
		storedBody := &storedBody{
			store:               self,
			memoryReservedBytes: memoryReservedBytes,
			knownLength:         knownLength,
		}
		self.memoryBytes += memoryReservedBytes
		self.bodies[storedBody] = true
		return storedBody, nil
	}()
	if refusal != nil {
		return nil, refusal
	}
	bodyMemoryBytesGauge.Add(float64(memoryReservedBytes))
	return storedBody, nil
}

// Reserves one spill file, or refuses with a local 503.
func (self *bodyStore) reserveSpillFile() *bodyRefusal {
	refusal := func() *bodyRefusal {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalClosed}
		}
		spillMaxFiles := self.settings.SpillMaxFiles
		if 0 < spillMaxFiles && spillMaxFiles <= self.spillFiles {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalSpillFiles}
		}
		self.spillFiles += 1
		return nil
	}()
	if refusal == nil {
		bodySpillFilesGauge.Inc()
	}
	return refusal
}

// Reserves spill bytes, or refuses with a local 503.
func (self *bodyStore) reserveSpillBytes(spillBytes int64) *bodyRefusal {
	refusal := func() *bodyRefusal {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalClosed}
		}
		spillMaxBytes := self.settings.SpillMaxBytes
		if 0 < spillMaxBytes && spillMaxBytes-self.spillBytes < spillBytes {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalSpillBytes}
		}
		self.spillBytes += spillBytes
		return nil
	}()
	if refusal == nil {
		bodySpillBytesGauge.Add(float64(spillBytes))
	}
	return refusal
}

// Returns a released body's reservations.
func (self *bodyStore) unreserve(storedBody *storedBody, memoryBytes int64, spillBytes int64, spillFiles int) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.memoryBytes -= memoryBytes
		self.spillBytes -= spillBytes
		self.spillFiles -= spillFiles
		delete(self.bodies, storedBody)
	}()
	bodyMemoryBytesGauge.Sub(float64(memoryBytes))
	bodySpillBytesGauge.Sub(float64(spillBytes))
	bodySpillFilesGauge.Sub(float64(spillFiles))
}

// What the store holds now: reserved memory, reserved spill bytes, spill
// files, and bodies not yet released.
func (self *bodyStore) usage() (memoryBytes int64, spillBytes int64, spillFiles int, bodies int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.memoryBytes, self.spillBytes, self.spillFiles, len(self.bodies)
}

// Releases every body still held, which closes its spill file and returns
// its budget, and refuses new bodies. A body still in use fails its next
// read.
func (self *bodyStore) close() error {
	storedBodies := func() []*storedBody {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		storedBodies := make([]*storedBody, 0, len(self.bodies))
		for storedBody := range self.bodies {
			storedBodies = append(storedBodies, storedBody)
		}
		return storedBodies
	}()
	for _, storedBody := range storedBodies {
		storedBody.release()
	}
	return nil
}

// One buffered body: a memory prefix and, past it, one spill file. Written
// by one read, then replayed by any number of readers; release may come from
// any goroutine. Safe for concurrent use.
type storedBody struct {
	store               *bodyStore
	memoryReservedBytes int64
	knownLength         bool
	// runs the release once, and holds every other call until it is done
	releaseOnce sync.Once

	stateLock          sync.Mutex
	prefix             []byte
	file               bodySpillFile
	fileSize           int64
	spillReservedBytes int64
	spillFileReserved  bool
	// closed when the spill file's creation has ended, with the file
	// recorded or closed; nil until the creation begins
	spillFileCreateDone chan struct{}
	released            bool
}

// Appends a chunk read from the client: the memory prefix first, then the
// spill file, under the body limit and the spill caps. Only the read that
// stores the body calls it. The file is created and written outside the
// lock, and every reservation is recorded under the lock first, so a release
// returns it. A release that comes in while the file is being created can't
// see the file, so it waits for the creation to end, and the append, finding
// the body released, closes the file; one that comes in later closes the
// file itself, so the write fails.
func (self *storedBody) append(chunk []byte) *bodyRefusal {
	closedRefusal := &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalClosed}
	file, spillChunk, refusal := func() (bodySpillFile, []byte, *bodyRefusal) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.released {
			return nil, nil, closedRefusal
		}
		maxBodyBytes := self.store.settings.MaxBodyBytes
		if 0 < maxBodyBytes && maxBodyBytes-self.lengthWithLock() < int64(len(chunk)) {
			return nil, nil, &bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge}
		}
		if room := self.memoryReservedBytes - int64(len(self.prefix)); 0 < room {
			if self.knownLength && self.prefix == nil {
				self.prefix = make([]byte, 0, self.memoryReservedBytes)
			}
			m := min(room, int64(len(chunk)))
			self.prefix = appendWithin(self.prefix, chunk[:m], self.memoryReservedBytes)
			chunk = chunk[m:]
		}
		return self.file, chunk, nil
	}()
	if refusal != nil || len(spillChunk) == 0 {
		return refusal
	}
	// records a reservation the store granted, unless the body was released
	recordReservation := func(record func()) bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.released {
			return false
		}
		record()
		return true
	}

	if file == nil {
		if refusal := self.store.reserveSpillFile(); refusal != nil {
			return refusal
		}
		spillFileCreateDone := make(chan struct{})
		if !recordReservation(func() {
			self.spillFileReserved = true
			self.spillFileCreateDone = spillFileCreateDone
		}) {
			self.store.unreserve(self, 0, 0, 1)
			return closedRefusal
		}
		createdFile, createRefusal := func() (bodySpillFile, *bodyRefusal) {
			// ends the creation on every exit, a panic included, so a
			// release never waits for good
			defer close(spillFileCreateDone)
			createdFile, err := self.store.createSpillFile()
			if err != nil {
				return nil, &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalSpillError}
			}
			if !recordReservation(func() { self.file = createdFile }) {
				createdFile.Close()
				return nil, closedRefusal
			}
			return createdFile, nil
		}()
		if createRefusal != nil {
			return createRefusal
		}
		file = createdFile
	}
	spillBytes := int64(len(spillChunk))
	if refusal := self.store.reserveSpillBytes(spillBytes); refusal != nil {
		return refusal
	}
	if !recordReservation(func() { self.spillReservedBytes += spillBytes }) {
		self.store.unreserve(self, 0, spillBytes, 0)
		return closedRefusal
	}
	n, err := file.Write(spillChunk)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.fileSize += int64(n)
	}()
	if err != nil || n < len(spillChunk) {
		return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalSpillError}
	}
	return nil
}

// Appends chunk to prefix, growing it at most to limit bytes of capacity.
func appendWithin(prefix []byte, chunk []byte, limit int64) []byte {
	need := len(prefix) + len(chunk)
	if need <= cap(prefix) {
		return append(prefix, chunk...)
	}
	grown := make([]byte, len(prefix), int(min(int64(max(2*cap(prefix), need)), limit)))
	copy(grown, prefix)
	return append(grown, chunk...)
}

// The bytes stored so far, memory and file.
func (self *storedBody) lengthWithLock() int64 {
	return int64(len(self.prefix)) + self.fileSize
}

// The stored length: what the upstream's Content-Length declares.
func (self *storedBody) length() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lengthWithLock()
}

// Whether any of the body is in a spill file.
func (self *storedBody) spilled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.file != nil
}

// A reader over the whole body from its start: memory, then file. Each
// attempt takes its own.
func (self *storedBody) newReader() io.Reader {
	return &storedBodyReader{
		storedBody: self,
	}
}

// Releases the body's memory, file and reservations. Safe to call more than
// once, and from any goroutine. Every call returns only once the body is
// released, whichever call released it, and the release waits for a spill
// file still being created, which its append closes. So the store's close,
// which can find a request's own release or append under way, returns with
// every spill file the body created closed, and each counted until then.
func (self *storedBody) release() {
	self.releaseOnce.Do(func() {
		file, spillFileCreateDone, spillReservedBytes, spillFiles := func() (bodySpillFile, chan struct{}, int64, int) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.released = true
			file := self.file
			self.file = nil
			self.prefix = nil
			spillFiles := 0
			if self.spillFileReserved {
				spillFiles = 1
			}
			return file, self.spillFileCreateDone, self.spillReservedBytes, spillFiles
		}()
		if spillFileCreateDone != nil {
			// a file created too late for the release to see is closed by
			// its append, which finds the body released
			<-spillFileCreateDone
		}
		if file != nil {
			file.Close()
		}
		self.store.unreserve(self, self.memoryReservedBytes, spillReservedBytes, spillFiles)
	})
}

// Reads a stored body from its start. Not safe for concurrent use; each
// attempt has its own.
type storedBodyReader struct {
	storedBody *storedBody
	offset     int64
}

// Reads on from the reader's offset, the memory prefix first, then the
// file at that offset. A released body fails with errBodyReleased, and a file
// shorter than what was written with io.ErrUnexpectedEOF.
func (self *storedBodyReader) Read(p []byte) (int, error) {
	prefix, file, fileSize, released := func() ([]byte, bodySpillFile, int64, bool) {
		self.storedBody.stateLock.Lock()
		defer self.storedBody.stateLock.Unlock()
		return self.storedBody.prefix, self.storedBody.file, self.storedBody.fileSize, self.storedBody.released
	}()
	if released {
		return 0, errBodyReleased
	}
	if len(p) == 0 {
		return 0, nil
	}
	if self.offset < int64(len(prefix)) {
		n := copy(p, prefix[self.offset:])
		self.offset += int64(n)
		return n, nil
	}
	fileOffset := self.offset - int64(len(prefix))
	if fileSize <= fileOffset {
		return 0, io.EOF
	}
	p = p[:min(int64(len(p)), fileSize-fileOffset)]
	n, err := file.ReadAt(p, fileOffset)
	self.offset += int64(n)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	if err == io.EOF {
		// the file is shorter than written: never a clean end
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
