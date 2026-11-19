package nginx

// Tests of body storage (D7, D19, D21): the memory prefix and its budget,
// the spill file and its caps, the body limit, the timeouts, replay, and
// release on every exit, directly and through a group.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

// A body reader that hands out its bytes in chunks and counts what was
// read, so a test can tell a refusal before reading from one after.
type countingReader struct {
	reader    io.Reader
	readBytes int
	reads     int
}

// Reads from the reader, counting the call and its bytes.
func (self *countingReader) Read(p []byte) (int, error) {
	self.reads += 1
	n, err := self.reader.Read(p)
	self.readBytes += n
	return n, err
}

// The store's usage, which must be zero once every body is released.
func checkStoreEmpty(t *testing.T, bodyStore *bodyStore) {
	t.Helper()
	if memoryBytes, spillBytes, spillFiles, bodies := bodyStore.usage(); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("store holds memory %d spill %d files %d bodies %d, want nothing", memoryBytes, spillBytes, spillFiles, bodies)
	}
}

// Reads a whole stored body.
func readStoredBody(t *testing.T, storedBody *storedBody) []byte {
	t.Helper()
	replay, err := io.ReadAll(storedBody.newReader())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return replay
}

// A spill file whose descriptors the test can see: each created file is
// recorded, and its writes fail past a quota when one is set.
type spySpillFiles struct {
	t          *testing.T
	dir        string
	quotaBytes int64

	stateLock sync.Mutex
	files     []*spySpillFile
}

// One spy spill file: a real file in the test directory, its writes
// counted against its spy's quota.
type spySpillFile struct {
	*os.File
	files        *spySpillFiles
	writtenBytes int64
}

// The failure a spy spill file's write returns past the quota.
var errInjectedQuota = errors.New("injected quota exceeded")

// Writes through to the file, or as much as the quota allows and then fails.
func (self *spySpillFile) Write(p []byte) (int, error) {
	if 0 < self.files.quotaBytes && self.files.quotaBytes < self.writtenBytes+int64(len(p)) {
		n, _ := self.File.Write(p[:self.files.quotaBytes-self.writtenBytes])
		self.writtenBytes += int64(n)
		return n, errInjectedQuota
	}
	n, err := self.File.Write(p)
	self.writtenBytes += int64(n)
	return n, err
}

// Creates and unlinks a spill file in the test directory, and records it.
func (self *spySpillFiles) create() (bodySpillFile, error) {
	file, err := os.CreateTemp(self.dir, "spy-body-*")
	if err != nil {
		return nil, err
	}
	os.Remove(file.Name())
	spyFile := &spySpillFile{File: file, files: self}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.files = append(self.files, spyFile)
	return spyFile, nil
}

// Every file created is closed.
func (self *spySpillFiles) checkClosed() {
	self.t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for i, file := range self.files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			self.t.Errorf("spill file %d is still open", i)
		}
	}
}

// The files created that are still open.
func (self *spySpillFiles) openCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	openCount := 0
	for _, file := range self.files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			openCount += 1
		}
	}
	return openCount
}

// The spill files created so far.
func (self *spySpillFiles) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.files)
}

// A store with spy spill files in a test directory.
func newSpyBodyStore(t *testing.T, settings *BodySettings) (*bodyStore, *spySpillFiles) {
	settings.SpillDir = t.TempDir()
	bodyStore := newBodyStore(settings)
	spyFiles := &spySpillFiles{t: t, dir: settings.SpillDir}
	bodyStore.createSpillFile = spyFiles.create
	t.Cleanup(func() { bodyStore.close() })
	return bodyStore, spyFiles
}

// The 64 KiB boundary: a body of exactly the prefix stays in memory, one
// byte more spills that byte, for a declared and an unknown length, and
// each replays byte for byte. The spill file is unlinked as it is created
// and closed on release.
func TestBodyStoreMemoryPrefixBoundary(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	prefixBytes := settings.MemoryPrefixBytes
	for _, c := range []struct {
		size        int64
		declared    bool
		wantSpilled int64
	}{
		{size: prefixBytes, declared: true, wantSpilled: 0},
		{size: prefixBytes + 1, declared: true, wantSpilled: 1},
		{size: prefixBytes, declared: false, wantSpilled: 0},
		{size: prefixBytes + 1, declared: false, wantSpilled: 1},
	} {
		body := testBodyBytes(int(c.size))
		declaredLength := int64(-1)
		if c.declared {
			declaredLength = c.size
		}
		storedBody, refusal := bodyStore.read(testContext(t), bytes.NewReader(body), bodyReadOptions{
			declaredLength: declaredLength,
			prefixBytes:    prefixBytes,
		})
		if refusal != nil {
			t.Fatalf("size %d: refused %v", c.size, refusal)
		}
		memoryBytes, spillBytes, spillFiles, _ := bodyStore.usage()
		if memoryBytes != prefixBytes || spillBytes != c.wantSpilled || (spillFiles == 1) != (0 < c.wantSpilled) {
			t.Errorf("size %d declared %v: memory %d spill %d files %d", c.size, c.declared, memoryBytes, spillBytes, spillFiles)
		}
		if storedBody.spilled() != (0 < c.wantSpilled) || storedBody.length() != c.size {
			t.Errorf("size %d: spilled %v length %d", c.size, storedBody.spilled(), storedBody.length())
		}
		if !bytes.Equal(readStoredBody(t, storedBody), body) || !bytes.Equal(readStoredBody(t, storedBody), body) {
			t.Errorf("size %d: a replay differs", c.size)
		}
		if entries, _ := os.ReadDir(settings.SpillDir); len(entries) != 0 {
			t.Errorf("size %d: spill file left a directory entry", c.size)
		}
		storedBody.release()
		storedBody.release()
		checkStoreEmpty(t, bodyStore)
	}
	if spyFiles.count() != 2 {
		t.Errorf("%d spill files, want one per spilled body", spyFiles.count())
	}
	spyFiles.checkClosed()
}

// A declared length over 16 MiB is a 413 before any read; exactly 16 MiB is
// stored.
func TestBodyStoreDeclaredLengthLimit(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	tooLarge := &countingReader{reader: bytes.NewReader([]byte("never read"))}
	if _, refusal := bodyStore.read(testContext(t), tooLarge, bodyReadOptions{
		declaredLength: settings.MaxBodyBytes + 1,
		prefixBytes:    settings.MemoryPrefixBytes,
	}); refusal == nil || refusal.status != http.StatusRequestEntityTooLarge || refusal.reason != bodyRefusalTooLarge {
		t.Fatalf("declared over the limit: %v, want 413", refusal)
	}
	if tooLarge.reads != 0 {
		t.Errorf("a declared 413 read the body")
	}
	checkStoreEmpty(t, bodyStore)

	body := testBodyBytes(int(settings.MaxBodyBytes))
	storedBody, refusal := bodyStore.read(testContext(t), bytes.NewReader(body), bodyReadOptions{
		declaredLength: settings.MaxBodyBytes,
		prefixBytes:    settings.MemoryPrefixBytes,
	})
	if refusal != nil {
		t.Fatalf("exactly the limit: refused %v", refusal)
	}
	if !bytes.Equal(readStoredBody(t, storedBody), body) {
		t.Errorf("a 16 MiB body replays differently")
	}
	storedBody.release()
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
}

// A body of unknown length that crosses 16 MiB is a 413 at that point,
// without reading on, and holds nothing after.
func TestBodyStoreUnknownLengthCrossesLimit(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	reader := &countingReader{reader: bytes.NewReader(testBodyBytes(int(settings.MaxBodyBytes) + 4*settings.ReadBufferBytes))}
	_, refusal := bodyStore.read(testContext(t), reader, bodyReadOptions{
		declaredLength: -1,
		prefixBytes:    settings.MemoryPrefixBytes,
	})
	if refusal == nil || refusal.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("crossing the limit: %v, want 413", refusal)
	}
	if int64(reader.readBytes) > settings.MaxBodyBytes+int64(settings.ReadBufferBytes) {
		t.Errorf("read %d bytes, past the read that crossed the limit", reader.readBytes)
	}
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
}

// A reader that hands out each chunk the test sends, across as many reads
// as it takes, and before it waits for the next chunk signals waiting: a
// barrier the test holds the reader at. A nil chunk is a failed read, as a
// closed client connection's is; a closed channel is the end.
type gatedReader struct {
	chunks    chan []byte
	waiting   chan struct{}
	remainder []byte
}

// The failure a gated reader's read returns for a nil chunk.
var errGatedReaderClosed = errors.New("client connection closed")

// Hands out the rest of the current chunk, or waits for the next one.
func (self *gatedReader) Read(p []byte) (int, error) {
	if len(self.remainder) == 0 {
		self.waiting <- struct{}{}
		chunk, ok := <-self.chunks
		if !ok {
			return 0, io.EOF
		}
		if chunk == nil {
			return 0, errGatedReaderClosed
		}
		self.remainder = chunk
	}
	n := copy(p, self.remainder)
	self.remainder = self.remainder[n:]
	return n, nil
}

// A body of unknown length reserves the whole memory prefix before its
// first read.
func TestBodyStoreUnknownLengthReservesBeforeFirstRead(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, _ := newSpyBodyStore(t, settings)
	reader := &gatedReader{chunks: make(chan []byte), waiting: make(chan struct{})}
	results := make(chan *storedBody, 1)
	go func() {
		storedBody, _ := bodyStore.read(testContext(t), reader, bodyReadOptions{
			declaredLength: -1,
			prefixBytes:    settings.MemoryPrefixBytes,
		})
		results <- storedBody
	}()
	receiveBounded(t, reader.waiting, "the body read")
	if memoryBytes, _, _, bodies := bodyStore.usage(); memoryBytes != settings.MemoryPrefixBytes || bodies != 1 {
		t.Errorf("before the first read: memory %d bodies %d, want the whole prefix reserved", memoryBytes, bodies)
	}
	reader.chunks <- []byte("ten bytes!")
	receiveBounded(t, reader.waiting, "the body read")
	close(reader.chunks)
	storedBody := receiveBounded(t, results, "stored body")
	if storedBody == nil || storedBody.length() != 10 {
		t.Fatalf("stored %v", storedBody)
	}
	storedBody.release()
	checkStoreEmpty(t, bodyStore)
}

// With the memory budget full, a body is a local 503 before its first read;
// once a body is released, the next one fits.
func TestBodyStoreMemoryBudget(t *testing.T) {
	settings := DefaultBodySettings()
	settings.MemoryBudgetBytes = settings.MemoryPrefixBytes + settings.MemoryPrefixBytes/2
	bodyStore, _ := newSpyBodyStore(t, settings)
	read := func(reader io.Reader, declaredLength int64) (*storedBody, *bodyRefusal) {
		return bodyStore.read(testContext(t), reader, bodyReadOptions{
			declaredLength: declaredLength,
			prefixBytes:    settings.MemoryPrefixBytes,
		})
	}
	held, refusal := read(bytes.NewReader(testBodyBytes(int(settings.MemoryPrefixBytes))), settings.MemoryPrefixBytes)
	if refusal != nil {
		t.Fatalf("first body refused: %v", refusal)
	}
	refusedReader := &countingReader{reader: bytes.NewReader(testBodyBytes(int(settings.MemoryPrefixBytes)))}
	if _, refusal := read(refusedReader, -1); refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.reason != bodyRefusalMemoryBudget {
		t.Fatalf("over the budget: %v, want a local 503", refusal)
	}
	if refusedReader.reads != 0 {
		t.Errorf("a budget refusal read the body")
	}
	// a small declared body still fits beside the held one
	small, refusal := read(bytes.NewReader([]byte("small")), 5)
	if refusal != nil {
		t.Fatalf("a small body refused: %v", refusal)
	}
	small.release()
	held.release()
	again, refusal := read(bytes.NewReader(testBodyBytes(int(settings.MemoryPrefixBytes))), -1)
	if refusal != nil {
		t.Fatalf("after release: refused %v", refusal)
	}
	again.release()
	checkStoreEmpty(t, bodyStore)
}

// Past SpillMaxBytes, or with SpillMaxFiles files open, a body is a local
// 503 and holds nothing after; a body that needs no file still fits.
func TestBodyStoreSpillCaps(t *testing.T) {
	settings := DefaultBodySettings()
	settings.SpillMaxBytes = 1000
	settings.SpillMaxFiles = 1
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	read := func(size int) (*storedBody, *bodyRefusal) {
		return bodyStore.read(testContext(t), bytes.NewReader(testBodyBytes(size)), bodyReadOptions{
			declaredLength: int64(size),
			prefixBytes:    settings.MemoryPrefixBytes,
		})
	}
	prefixBytes := int(settings.MemoryPrefixBytes)
	if _, refusal := read(prefixBytes + 1001); refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.reason != bodyRefusalSpillBytes {
		t.Fatalf("over the spill bytes: %v, want a local 503", refusal)
	}
	checkStoreEmpty(t, bodyStore)

	held, refusal := read(prefixBytes + 1000)
	if refusal != nil {
		t.Fatalf("at the spill bytes: refused %v", refusal)
	}
	if _, refusal := read(prefixBytes + 1); refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.reason != bodyRefusalSpillFiles {
		t.Fatalf("over the spill files: %v, want a local 503", refusal)
	}
	inMemory, refusal := read(prefixBytes)
	if refusal != nil {
		t.Fatalf("a body with no spill refused: %v", refusal)
	}
	inMemory.release()
	held.release()
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
}

// A spill write that fails under a small injected quota is a local 503,
// and the body's file and reservations are released.
func TestBodyStoreSpillWriteFailure(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	spyFiles.quotaBytes = 100
	_, refusal := bodyStore.read(testContext(t), bytes.NewReader(testBodyBytes(int(settings.MemoryPrefixBytes)+1000)), bodyReadOptions{
		declaredLength: -1,
		prefixBytes:    settings.MemoryPrefixBytes,
	})
	if refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.reason != bodyRefusalSpillError {
		t.Fatalf("failed spill write: %v, want a local 503", refusal)
	}
	if spyFiles.count() != 1 {
		t.Fatalf("%d spill files, want 1", spyFiles.count())
	}
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
}

// A body whose reads end, without an error, short of its declared length, as
// the vendored http3 ends one at the stream's end, is nginx's 400, and
// nothing stays held; one that ends at its length, whether that read reports
// the end or not, is stored.
func TestBodyStoreRefusesABodyEndingShortOfItsLength(t *testing.T) {
	settings := DefaultBodySettings()
	for _, c := range []struct {
		name           string
		body           io.Reader
		declaredLength int64
		wantRefused    bool
	}{
		{name: "short", body: bytes.NewReader(testBodyBytes(10)), declaredLength: 100, wantRefused: true},
		{name: "short with its end", body: iotest.DataErrReader(bytes.NewReader(testBodyBytes(10))), declaredLength: 100, wantRefused: true},
		{name: "short past the prefix", body: bytes.NewReader(testBodyBytes(int(settings.MemoryPrefixBytes) + 10)), declaredLength: 2 * settings.MemoryPrefixBytes, wantRefused: true},
		{name: "empty", body: bytes.NewReader(nil), declaredLength: 1, wantRefused: true},
		{name: "whole", body: bytes.NewReader(testBodyBytes(100)), declaredLength: 100},
		{name: "whole with its end", body: iotest.DataErrReader(bytes.NewReader(testBodyBytes(100))), declaredLength: 100},
		{name: "of unknown length", body: bytes.NewReader(testBodyBytes(10)), declaredLength: -1},
	} {
		bodyStore, spyFiles := newSpyBodyStore(t, settings)
		storedBody, refusal := bodyStore.read(testContext(t), c.body, bodyReadOptions{
			declaredLength: c.declaredLength,
			prefixBytes:    settings.MemoryPrefixBytes,
		})
		if c.wantRefused {
			if refusal == nil || refusal.status != http.StatusBadRequest || refusal.reason != bodyRefusalIncomplete {
				t.Errorf("%s: %v, want a 400", c.name, refusal)
			}
		} else {
			if refusal != nil {
				t.Errorf("%s: refused %v", c.name, refusal)
			} else {
				storedBody.release()
			}
		}
		checkStoreEmpty(t, bodyStore)
		spyFiles.checkClosed()
	}
}

// A client that goes away while its body spills: nothing to answer, and
// nothing held.
func TestBodyStoreCancellationDuringSpill(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &gatedReader{chunks: make(chan []byte), waiting: make(chan struct{})}
	results := make(chan *bodyRefusal, 1)
	go func() {
		_, refusal := bodyStore.read(ctx, reader, bodyReadOptions{
			declaredLength: -1,
			prefixBytes:    settings.MemoryPrefixBytes,
		})
		results <- refusal
	}()
	receiveBounded(t, reader.waiting, "the body read")
	reader.chunks <- testBodyBytes(int(settings.MemoryPrefixBytes))
	receiveBounded(t, reader.waiting, "the body read")
	reader.chunks <- testBodyBytes(500)
	receiveBounded(t, reader.waiting, "the body read")
	// the body has spilled 500 bytes and is waiting for more
	if _, spillBytes, spillFiles, _ := bodyStore.usage(); spillBytes != 500 || spillFiles != 1 {
		t.Fatalf("spilled %d bytes in %d files before the cancel", spillBytes, spillFiles)
	}
	cancel()
	// the client's connection ends the pending read
	reader.chunks <- nil
	refusal := receiveBounded(t, results, "refusal")
	if refusal == nil || refusal.status != 0 || refusal.reason != bodyRefusalCanceled {
		t.Fatalf("canceled read: %v, want no answer", refusal)
	}
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
}

// A read past client_body_timeout is a 408, even though the server cancels
// the request's context when the read times out. Each read is bounded by a
// fresh deadline; a body read whole clears it, and a failed read leaves it,
// so the server never waits on the client to discard the rest. A client
// that ends early is a 400.
func TestBodyStoreReadFailures(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, _ := newSpyBodyStore(t, settings)
	deadlines := []time.Time{}
	setReadDeadline := func(deadline time.Time) error {
		deadlines = append(deadlines, deadline)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	timeoutReader := io.MultiReader(bytes.NewReader([]byte("partial")), errorReader{err: os.ErrDeadlineExceeded, cancel: cancel})
	_, refusal := bodyStore.read(ctx, timeoutReader, bodyReadOptions{
		declaredLength:  100,
		prefixBytes:     settings.MemoryPrefixBytes,
		readTimeout:     settings.ClientBodyTimeout,
		setReadDeadline: setReadDeadline,
	})
	if refusal == nil || refusal.status != http.StatusRequestTimeout || refusal.reason != bodyRefusalTimeout {
		t.Fatalf("timed out read: %v, want 408", refusal)
	}
	if len(deadlines) != 2 || deadlines[0].IsZero() || deadlines[1].IsZero() {
		t.Errorf("read deadlines %v, want one per read and none cleared", deadlines)
	}
	checkStoreEmpty(t, bodyStore)

	deadlines = deadlines[:0]
	storedBody, refusal := bodyStore.read(testContext(t), bytes.NewReader([]byte("whole")), bodyReadOptions{
		declaredLength:  5,
		prefixBytes:     settings.MemoryPrefixBytes,
		readTimeout:     settings.ClientBodyTimeout,
		setReadDeadline: setReadDeadline,
	})
	if refusal != nil {
		t.Fatalf("whole body refused: %v", refusal)
	}
	storedBody.release()
	if len(deadlines) < 2 || deadlines[0].IsZero() || !deadlines[len(deadlines)-1].IsZero() {
		t.Errorf("read deadlines %v, want one per read, then cleared", deadlines)
	}

	shortReader := io.MultiReader(bytes.NewReader([]byte("short")), errorReader{err: io.ErrUnexpectedEOF})
	if _, refusal := bodyStore.read(testContext(t), shortReader, bodyReadOptions{
		declaredLength: 100,
		prefixBytes:    settings.MemoryPrefixBytes,
	}); refusal == nil || refusal.status != http.StatusBadRequest || refusal.reason != bodyRefusalIncomplete {
		t.Fatalf("early end: %v, want 400", refusal)
	}
	checkStoreEmpty(t, bodyStore)
}

// A reader that fails, first canceling the request's context when given a
// cancel, as Go's HTTP/1.1 server does on a failed read.
type errorReader struct {
	err    error
	cancel context.CancelFunc
}

// Fails at once, canceling first when it has a cancel.
func (self errorReader) Read(p []byte) (int, error) {
	if self.cancel != nil {
		self.cancel()
	}
	return 0, self.err
}

// Release runs on every exit: a panic while a body is in use, and the
// store's close, which also fails a reader still using the body and
// refuses new bodies.
func TestBodyStoreReleaseOnEveryExit(t *testing.T) {
	settings := DefaultBodySettings()
	bodyStore, spyFiles := newSpyBodyStore(t, settings)
	body := testBodyBytes(int(settings.MemoryPrefixBytes) + 10)
	read := func() *storedBody {
		storedBody, refusal := bodyStore.read(testContext(t), bytes.NewReader(body), bodyReadOptions{
			declaredLength: int64(len(body)),
			prefixBytes:    settings.MemoryPrefixBytes,
		})
		if refusal != nil {
			t.Fatalf("refused %v", refusal)
		}
		return storedBody
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("no panic")
			}
		}()
		storedBody := read()
		defer storedBody.release()
		panic("handler panic")
	}()
	checkStoreEmpty(t, bodyStore)

	inUse := read()
	reader := inUse.newReader()
	if _, err := reader.Read(make([]byte, 10)); err != nil {
		t.Fatalf("first read: %v", err)
	}
	bodyStore.close()
	if _, err := reader.Read(make([]byte, 10)); !errors.Is(err, errBodyReleased) {
		t.Errorf("a read after close: %v, want errBodyReleased", err)
	}
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
	if _, refusal := bodyStore.read(testContext(t), bytes.NewReader(body), bodyReadOptions{declaredLength: -1}); refusal == nil || refusal.reason != bodyRefusalClosed {
		t.Errorf("a read after close: %v, want refused", refusal)
	}
}

// Through a group: with the budget held by one request, the next is a
// local 503 before its body is read, counted as a refusal and never sent
// to a block, while the held request still completes.
func TestForwardBodyBudgetIsLocal503(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Body.MemoryBudgetBytes = settings.Body.MemoryPrefixBytes
	serviceName := testServiceName("body-budget")
	release := make(chan struct{})
	// held handlers end on any exit, so the servers' cleanup never waits
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	arrivals := make(chan *recordedRequest)
	upstream := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		<-release
		io.WriteString(w, "held")
	})
	upstream.arrivals = arrivals
	other := newRecordingUpstream(t, "g2", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
		{name: "g2", weight: 1, port: other.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)

	heldResponses := make(chan *http.Response, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, front.url("/held"), bytes.NewReader(testBodyBytes(70*1024)))
		response, _, _ := testDo(t, request)
		heldResponses <- response
	}()
	receiveBounded(t, arrivals, "arrival at g1")

	request, _ := http.NewRequest(http.MethodPost, front.url("/refused"), bytes.NewReader([]byte("x")))
	response, _, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over the budget: %v %v, want 503", response, err)
	}
	if bodyRefusalCount(serviceName, bodyRefusalMemoryBudget) != 1 {
		t.Errorf("the local 503 was not counted as a refusal")
	}
	if len(other.recorded()) != 0 || attemptCount(serviceName, "g2", "response") != 0 || retryCount(serviceName, "http_503") != 0 {
		t.Errorf("a local 503 reached a block or was retried")
	}
	releaseHeld()
	if response := receiveBounded(t, heldResponses, "held response"); response == nil || response.StatusCode != http.StatusOK {
		t.Errorf("held request: %v", response)
	}
	front.waitServed(t)
	front.waitServed(t)
	checkStoreEmpty(t, bodyStore)
}

// Through a group: a declared length over the limit is a 413 before any
// read or upstream. A stall between body reads gets no response at all, as
// nginx terminates a request whose body timed out: the handler aborts, and
// Go's server closes the connection with nothing written. Neither reaches a
// block, both are counted, and neither holds anything after.
func TestForwardBodyLimitAndTimeout(t *testing.T) {
	serviceName := testServiceName("body-limit-timeout")
	upstream := newRecordingUpstream(t, "g1", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.clientBodyTimeout = 50 * time.Millisecond
		return options
	})

	response, _ := rawFrontRequest(t, front, "POST /upload HTTP/1.1\r\nHost: upload.service.example\r\nContent-Length: "+strconv.Itoa(16*1024*1024+1)+"\r\n\r\n")
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared over the limit: %d, want 413", response.StatusCode)
	}

	conn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /upload HTTP/1.1\r\nHost: upload.service.example\r\nContent-Length: 100\r\n\r\nonly ten b")
	// a bound, so a held connection fails the read rather than hangs it
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if answered, err := io.ReadAll(conn); len(answered) != 0 || (err != nil && !isClosedError(err)) {
		t.Fatalf("the stalled body was answered %q %v, want the connection closed with nothing written", answered, err)
	}
	front.waitServed(t)
	if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
		t.Errorf("the stalled body's serveHttp ended with %v, want http.ErrAbortHandler", recovered)
	}
	if len(upstream.recorded()) != 0 {
		t.Errorf("a refused body reached the block")
	}
	if bodyRefusalCount(serviceName, bodyRefusalTooLarge) != 1 || bodyRefusalCount(serviceName, bodyRefusalTimeout) != 1 {
		t.Errorf("refusals not counted")
	}
	checkStoreEmpty(t, bodyStore)
}

// Through a group: a client that goes away while its spilled body is being
// replayed to a block ends the attempt, and the body's memory, file and
// reservations are released.
func TestForwardCancellationDuringReplay(t *testing.T) {
	settings := testForwardFrontSettings()
	serviceName := testServiceName("body-replay-cancel")
	gotPart := make(chan struct{})
	release := make(chan struct{})
	upstream := newRecordingUpstream(t, "g1", nil)
	// a block that reads a little of the body, then stops reading
	upstream.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadFull(r.Body, make([]byte, 1024))
		close(gotPart)
		<-release
	})
	defer close(release)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	spyFiles := &spySpillFiles{t: t, dir: t.TempDir()}
	bodyStore.createSpillFile = spyFiles.create
	front := newTestForwardFront(t, group, testForwardOptions)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.url("/upload"), bytes.NewReader(testBodyBytes(8*1024*1024)))
		_, _, err := testDo(t, request)
		results <- err
	}()
	<-gotPart
	if _, _, spillFiles, bodies := bodyStore.usage(); spillFiles != 1 || bodies != 1 {
		t.Fatalf("during the replay: %d files %d bodies, want the spilled body held", spillFiles, bodies)
	}
	cancel()
	if err := receiveBounded(t, results, "result"); err == nil {
		t.Errorf("the canceled request succeeded")
	}
	front.waitServed(t)
	checkStoreEmpty(t, bodyStore)
	spyFiles.checkClosed()
}

// Through a group: a response the block cuts short aborts the handler with
// a panic, and the stored body is still released.
func TestForwardAbortReleasesBody(t *testing.T) {
	raw := newRawUpstream(t)
	group, bodyStore := newTestGroup(t, testForwardRouteService("body-abort", []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, front.url("/"), bytes.NewReader(testBodyBytes(100*1024)))
		testDo(t, request)
	}()
	conn, reader, request := raw.accept(t)
	io.Copy(io.Discard, io.LimitReader(reader, request.ContentLength))
	io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\ncut")
	conn.Close()
	if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
		t.Fatalf("serveHttp ended with %v, want http.ErrAbortHandler", recovered)
	}
	checkStoreEmpty(t, bodyStore)
}

// Through a group: a chunked body over the body limit on a streamed route is
// a 413 while nothing was answered. It streams, chunked, though it came whole
// with its head, so it is refused as it crosses the limit: its block's
// request is cut, and the attempt is the client's.
func TestForwardStreamedBodyCrossesLimit(t *testing.T) {
	chunk := string(testBodyBytes(600))
	settings := testForwardFrontSettings()
	settings.Body.MaxBodyBytes = 1000
	serviceName := testServiceName("streamed-limit")
	upstream := newRecordingUpstream(t, "g1", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	response, _ := rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nTransfer-Encoding: chunked\r\n\r\n258\r\n"+chunk+"\r\n258\r\n"+chunk+"\r\n0\r\n\r\n")
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over the limit: %d, want 413", response.StatusCode)
	}
	front.waitServed(t)
	if bodyRefusalCount(serviceName, bodyRefusalTooLarge) != 1 {
		t.Errorf("the 413 was not counted")
	}
	if attemptCount(serviceName, "g1", "client") != 1 || attemptCount(serviceName, "g1", "response") != 0 {
		t.Errorf("%v attempts ended on the client's side, want 1, and none answered", attemptCount(serviceName, "g1", "client"))
	}
	for _, recorded := range upstream.recorded() {
		if recorded.bodyErr == nil {
			t.Errorf("the block got a whole body of %d bytes", len(recorded.body))
		}
	}
	checkStoreEmpty(t, bodyStore)
}

// The store's close, which the front's forced close runs, returns only once
// every spill file its bodies created is closed, one still being created
// included. A close that releases a body while its append is creating the
// file can't see the file: it waits for the append, which finds the body
// released and closes the file, and the file stays counted against
// SpillMaxFiles until then (D21). In a synctest bubble, so the close is seen
// waiting rather than given time to return.
func TestBodyStoreCloseWaitsForASpillFileBeingCreated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultBodySettings()
		bodyStore, spyFiles := newSpyBodyStore(t, settings)
		created := make(chan struct{})
		proceed := make(chan struct{})
		bodyStore.createSpillFile = func() (bodySpillFile, error) {
			file, err := spyFiles.create()
			if err != nil {
				return nil, err
			}
			close(created)
			// the file is open, and its body hasn't recorded it
			<-proceed
			return file, nil
		}
		refusals := make(chan *bodyRefusal, 1)
		go func() {
			body := testBodyBytes(int(settings.MemoryPrefixBytes) + 1000)
			storedBody, refusal := bodyStore.read(t.Context(), bytes.NewReader(body), bodyReadOptions{
				declaredLength: int64(len(body)),
				prefixBytes:    settings.MemoryPrefixBytes,
			})
			if storedBody != nil {
				storedBody.release()
			}
			refusals <- refusal
		}()
		<-created

		closeDone := make(chan struct{})
		go func() {
			bodyStore.close()
			close(closeDone)
		}()
		// every other goroutine is durably blocked: the append at proceed,
		// and the close returned or waiting for the append
		synctest.Wait()
		select {
		case <-closeDone:
			if spyFiles.openCount() != 0 {
				t.Errorf("the store's close returned while a spill file being created was open")
			}
		default:
		}
		if _, _, spillFiles, _ := bodyStore.usage(); spillFiles != 1 {
			t.Errorf("with a spill file open, the store counts %d spill files, want 1", spillFiles)
		}

		close(proceed)
		<-closeDone
		spyFiles.checkClosed()
		if refusal := <-refusals; refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.reason != bodyRefusalClosed {
			t.Errorf("the body read the close cut short: %v, want refused as closed", refusal)
		}
		checkStoreEmpty(t, bodyStore)
	})
}
