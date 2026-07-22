package stats

// streamWriter owns one stats stream's on-disk segments.
//
// Appended messages arrive on a bounded channel; a full channel drops the
// message and counts it (Append must never block its caller — FindProviders2
// is a hot path). A single writer goroutine drains the channel, marshals each
// message, and writes it as a length-delimited frame into the current segment.
//
// Segments are streaming-zstd files rotated by size or age. A segment being
// written is named `<name>.pb.zst.partial`; on rotation/close it is fsync'd
// and renamed to `<name>.pb.zst`. The uploader/export only ever read the
// final `.pb.zst` names, so a partial (including one left by a crash) is
// never shipped.
//
// A streamWriter's methods are not safe for concurrent use except Append,
// which is; the owning Stats serializes lifecycle calls.

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

const partialSuffix = ".partial"
const segmentExt = ".pb.zst"

type streamWriterSettings struct {
	maxSegmentBytes int64
	maxSegmentAge   time.Duration
	// bounded queue depth before Append drops
	queueSize     int
	maxQueueBytes int64
}

func defaultStreamWriterSettings() *streamWriterSettings {
	return &streamWriterSettings{
		maxSegmentBytes: 64 * 1024 * 1024,
		maxSegmentAge:   5 * time.Minute,
		queueSize:       4096,
		maxQueueBytes:   32 * 1024 * 1024,
	}
}

type queuedMessage struct {
	message proto.Message
	size    int64
}

type streamWriter struct {
	ctx      context.Context
	dir      string
	stream   string
	settings *streamWriterSettings

	messages    chan queuedMessage
	queuedBytes atomic.Int64
	dropped     atomic.Uint64

	closeOnce sync.Once
	done      chan struct{}

	// writer-goroutine-owned segment state
	encoder      *zstd.Encoder
	file         *os.File
	partialPath  string
	finalPath    string
	segmentBytes int64
	segmentStart time.Time
	// monotonic sequence for unique segment names within a start-millisecond
	seq uint64
	// scratch frame-length header, reused per write
	lenBuf [4]byte
}

func newStreamWriter(ctx context.Context, dir string, stream string, settings *streamWriterSettings) (*streamWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	self := &streamWriter{
		ctx:      ctx,
		dir:      dir,
		stream:   stream,
		settings: settings,
		messages: make(chan queuedMessage, settings.queueSize),
		done:     make(chan struct{}),
	}
	go server.HandleError(self.run)
	return self, nil
}

// append enqueues a message, dropping (and counting) it if the queue is full.
// Safe for concurrent use.
func (self *streamWriter) append(message proto.Message) {
	size := int64(proto.Size(message))
	if !self.reserveQueueBytes(size) {
		self.dropped.Add(1)
		return
	}
	select {
	case self.messages <- queuedMessage{message: message, size: size}:
	default:
		self.queuedBytes.Add(-size)
		self.dropped.Add(1)
	}
}

func (self *streamWriter) reserveQueueBytes(size int64) bool {
	if size < 0 || self.settings.maxQueueBytes < size {
		return false
	}
	for {
		current := self.queuedBytes.Load()
		if self.settings.maxQueueBytes-current < size {
			return false
		}
		if self.queuedBytes.CompareAndSwap(current, current+size) {
			return true
		}
	}
}

func (self *streamWriter) droppedCount() uint64 {
	return self.dropped.Load()
}

// run drains the queue until the context is done or close is called, then
// finalizes the open segment.
func (self *streamWriter) run() {
	defer close(self.done)
	defer self.finalizeSegment()

	// periodic age-based rotation check even when idle
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-self.ctx.Done():
			return
		case queued, ok := <-self.messages:
			if !ok {
				return
			}
			self.write(queued.message)
			self.queuedBytes.Add(-queued.size)
			if self.shouldRotate() {
				self.finalizeSegment()
			}
		case <-ticker.C:
			if self.file != nil && self.settings.maxSegmentAge <= time.Since(self.segmentStart) {
				self.finalizeSegment()
			}
		}
	}
}

func (self *streamWriter) write(message proto.Message) {
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		glog.Infof("[stats]%s marshal err=%s\n", self.stream, err)
		return
	}
	if self.file == nil {
		if err := self.openSegment(); err != nil {
			glog.Infof("[stats]%s open segment err=%s\n", self.stream, err)
			return
		}
	}
	binary.BigEndian.PutUint32(self.lenBuf[:], uint32(len(messageBytes)))
	if _, err := self.encoder.Write(self.lenBuf[:]); err != nil {
		glog.Infof("[stats]%s write len err=%s\n", self.stream, err)
		self.discardSegment()
		return
	}
	if _, err := self.encoder.Write(messageBytes); err != nil {
		glog.Infof("[stats]%s write frame err=%s\n", self.stream, err)
		self.discardSegment()
		return
	}
	self.segmentBytes += int64(len(self.lenBuf)) + int64(len(messageBytes))
}

func (self *streamWriter) openSegment() error {
	self.seq += 1
	name := fmt.Sprintf("%d-%d%s", time.Now().UnixMilli(), self.seq, segmentExt)
	self.finalPath = filepath.Join(self.dir, name)
	self.partialPath = self.finalPath + partialSuffix

	file, err := os.Create(self.partialPath)
	if err != nil {
		return err
	}
	encoder, err := zstd.NewWriter(file)
	if err != nil {
		file.Close()
		os.Remove(self.partialPath)
		return err
	}
	self.file = file
	self.encoder = encoder
	self.segmentBytes = 0
	self.segmentStart = time.Now()
	return nil
}

func (self *streamWriter) shouldRotate() bool {
	if self.file == nil {
		return false
	}
	return self.settings.maxSegmentBytes <= self.segmentBytes ||
		self.settings.maxSegmentAge <= time.Since(self.segmentStart)
}

// finalizeSegment closes the current segment and renames it to its final
// name so consumers can pick it up. No-op when no segment is open.
func (self *streamWriter) finalizeSegment() {
	if self.file == nil {
		return
	}
	ok := true
	if err := self.encoder.Close(); err != nil {
		glog.Infof("[stats]%s encoder close err=%s\n", self.stream, err)
		ok = false
	}
	if err := self.file.Sync(); err != nil {
		glog.Infof("[stats]%s sync err=%s\n", self.stream, err)
	}
	if err := self.file.Close(); err != nil {
		glog.Infof("[stats]%s file close err=%s\n", self.stream, err)
		ok = false
	}
	if ok {
		if err := os.Rename(self.partialPath, self.finalPath); err != nil {
			glog.Infof("[stats]%s rename err=%s\n", self.stream, err)
		}
	} else {
		os.Remove(self.partialPath)
	}
	self.file = nil
	self.encoder = nil
	self.segmentBytes = 0
}

// discardSegment drops a segment that hit a write error, without publishing it.
func (self *streamWriter) discardSegment() {
	if self.file == nil {
		return
	}
	self.encoder.Close()
	self.file.Close()
	os.Remove(self.partialPath)
	self.file = nil
	self.encoder = nil
	self.segmentBytes = 0
}

// close stops accepting messages and waits for the writer goroutine to
// finalize the open segment.
func (self *streamWriter) close() {
	self.closeOnce.Do(func() {
		close(self.messages)
	})
	<-self.done
}
