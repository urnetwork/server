package nginx

// Request body storage for buffered forwarding (owner: s-forward; D7, D19,
// D21). nginx reads a buffered body whole before it picks an upstream, so a
// retry can send it again; the front does the same:
//   - a body keeps its first bytes in memory: min(its declared length, its
//     route's client_body_buffer_size or BodySettings.MemoryPrefixBytes), the
//     whole prefix for a body of unknown length, reserved from the process's
//     BodySettings.MemoryBudgetBytes before the first read (D7, D19). With no
//     room, the front answers a local 503 before reading anything;
//   - the bytes past the prefix go to one spill file in BodySettings.SpillDir,
//     taken under BodySettings.SpillMaxFiles when the first such byte
//     arrives, and reserved under BodySettings.SpillMaxBytes as bytes arrive
//     (D21). Past either cap, or when the file can't be created or written,
//     a local 503;
//   - a declared length over BodySettings.MaxBodyBytes is a 413 before
//     reading; a body of unknown length that crosses it is a 413 at that
//     point;
//   - a read that waits longer than client_body_timeout is a 408, and a
//     client that ends its body early a 400;
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
	"time"
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
	// 408: a read waited past client_body_timeout
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
}

// Reads a whole body into the store, under the rules in the file comment.
// On a refusal nothing stays reserved or open. The client's read deadline
// is cleared when the body has been read whole, so nothing times out the
// connection while the upstream answers; after a failed read it stays, so
// the server closes the connection rather than wait on the client.
func (self *bodyStore) read(ctx context.Context, body io.Reader, options bodyReadOptions) (*storedBody, *bodyRefusal) {
	maxBodyBytes := self.settings.MaxBodyBytes
	if 0 < maxBodyBytes && maxBodyBytes < options.declaredLength {
		return nil, &bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge}
	}
	memoryReservedBytes := max(options.prefixBytes, 0)
	if 0 <= options.declaredLength {
		memoryReservedBytes = min(memoryReservedBytes, options.declaredLength)
	}
	storedBody, refusal := self.open(memoryReservedBytes, 0 <= options.declaredLength)
	if refusal != nil {
		return nil, refusal
	}
	stored := false
	defer func() {
		if !stored {
			storedBody.release()
		}
	}()

	readBuffer := self.readBuffers.Get().(*[]byte)
	defer self.readBuffers.Put(readBuffer)
	for {
		if ctx.Err() != nil {
			return nil, &bodyRefusal{status: 0, reason: bodyRefusalCanceled}
		}
		if 0 < options.readTimeout && options.setReadDeadline != nil {
			options.setReadDeadline(time.Now().Add(options.readTimeout))
		}
		n, err := body.Read(*readBuffer)
		if 0 < n {
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
	if 0 < options.readTimeout && options.setReadDeadline != nil {
		// nothing reads the client again until its next request
		options.setReadDeadline(time.Time{})
	}
	stored = true
	return storedBody, nil
}

// The refusal for a failed client body read: its read timed out (408), the
// client went away, or its body ended early or broke its framing (400). The
// timeout is checked first: Go's HTTP/1.1 server cancels the request's
// context on any failed read, a timed out one included.
func bodyReadRefusal(ctx context.Context, err error) *bodyRefusal {
	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &bodyRefusal{status: http.StatusRequestTimeout, reason: bodyRefusalTimeout}
	}
	if ctx.Err() != nil {
		return &bodyRefusal{status: 0, reason: bodyRefusalCanceled}
	}
	return &bodyRefusal{status: http.StatusBadRequest, reason: bodyRefusalIncomplete}
}

// Registers a new body with its memory prefix reserved, or refuses with a
// local 503. A known length allocates its prefix at once; an unknown one
// grows it as bytes arrive, within the reservation.
func (self *bodyStore) open(memoryReservedBytes int64, knownLength bool) (*storedBody, *bodyRefusal) {
	storedBody := &storedBody{
		store:               self,
		memoryReservedBytes: memoryReservedBytes,
	}
	if knownLength {
		storedBody.prefix = make([]byte, 0, memoryReservedBytes)
	}
	refusal := func() *bodyRefusal {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalClosed}
		}
		memoryBudgetBytes := self.settings.MemoryBudgetBytes
		if 0 < memoryBudgetBytes && memoryBudgetBytes-self.memoryBytes < memoryReservedBytes {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalMemoryBudget}
		}
		self.memoryBytes += memoryReservedBytes
		self.bodies[storedBody] = true
		return nil
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

	stateLock          sync.Mutex
	prefix             []byte
	file               bodySpillFile
	fileSize           int64
	spillReservedBytes int64
	spillFileReserved  bool
	released           bool
}

// Appends a chunk read from the client: the memory prefix first, then the
// spill file, under the body limit and the spill caps.
func (self *storedBody) append(chunk []byte) *bodyRefusal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.released {
		return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalClosed}
	}
	maxBodyBytes := self.store.settings.MaxBodyBytes
	if 0 < maxBodyBytes && maxBodyBytes-self.lengthWithLock() < int64(len(chunk)) {
		return &bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge}
	}

	if room := self.memoryReservedBytes - int64(len(self.prefix)); 0 < room {
		m := min(room, int64(len(chunk)))
		self.prefix = appendWithin(self.prefix, chunk[:m], self.memoryReservedBytes)
		chunk = chunk[m:]
	}
	if len(chunk) == 0 {
		return nil
	}

	if self.file == nil {
		if refusal := self.store.reserveSpillFile(); refusal != nil {
			return refusal
		}
		self.spillFileReserved = true
		file, err := self.store.createSpillFile()
		if err != nil {
			return &bodyRefusal{status: http.StatusServiceUnavailable, reason: bodyRefusalSpillError}
		}
		self.file = file
	}
	if refusal := self.store.reserveSpillBytes(int64(len(chunk))); refusal != nil {
		return refusal
	}
	self.spillReservedBytes += int64(len(chunk))
	n, err := self.file.Write(chunk)
	self.fileSize += int64(n)
	if err != nil || n < len(chunk) {
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
// once, and from any goroutine.
func (self *storedBody) release() {
	file, spillReservedBytes, spillFiles, released := func() (bodySpillFile, int64, int, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.released {
			return nil, 0, 0, true
		}
		self.released = true
		file := self.file
		self.file = nil
		self.prefix = nil
		spillFiles := 0
		if self.spillFileReserved {
			spillFiles = 1
		}
		return file, self.spillReservedBytes, spillFiles, false
	}()
	if released {
		return
	}
	if file != nil {
		file.Close()
	}
	self.store.unreserve(self, self.memoryReservedBytes, spillReservedBytes, spillFiles)
}

// Reads a stored body from its start. Not safe for concurrent use; each
// attempt has its own.
type storedBodyReader struct {
	storedBody *storedBody
	offset     int64
}

func (self *storedBodyReader) Read(p []byte) (int, error) {
	prefix, file, fileSize, released := func() ([]byte, bodySpillFile, int64, bool) {
		self.storedBody.stateLock.Lock()
		defer self.storedBody.stateLock.Unlock()
		return self.storedBody.prefix, self.storedBody.file, self.storedBody.fileSize, self.storedBody.released
	}()
	if released {
		return 0, errBodyReleased
	}
	if self.offset < int64(len(prefix)) {
		n := copy(p, prefix[self.offset:])
		self.offset += int64(n)
		return n, nil
	}
	fileOffset := self.offset - int64(len(prefix))
	if fileSize <= fileOffset || len(p) == 0 {
		if fileSize <= fileOffset {
			return 0, io.EOF
		}
		return 0, nil
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
