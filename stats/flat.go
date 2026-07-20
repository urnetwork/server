package stats

// flat sample export: the whole FindProviders2 sample corpus as ONE file
// instead of a tarball of per-instance segments. The framing is the standard
// varint-length-delimited protobuf format (google.golang.org/protobuf
// protodelim — every language's protobuf runtime can read it: a base-128
// varint length followed by that many bytes of a FindProviders2Sample), and
// the whole stream is xz-compressed.
//
//	<xz>( repeated: <uvarint length><marshaled FindProviders2Sample> )
//
// This is the format handed to competitors (the official dump from main and
// the evaluation dump), and the input to the diff tool (metrics.go).

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"

	"github.com/ulikunitz/xz"
	"google.golang.org/protobuf/encoding/protodelim"

	"github.com/urnetwork/server/stats/sample"
)

// FlatWriter writes varint-delimited frames to an xz stream.
type FlatWriter struct {
	file   *os.File
	xz     *xz.Writer
	buffer *bufio.Writer
	lenBuf [binary.MaxVarintLen64]byte
	count  int
}

// NewFlatWriter creates the output .xz file.
func NewFlatWriter(path string) (*FlatWriter, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	buffer := bufio.NewWriterSize(file, 1<<20)
	xzWriter, err := xz.NewWriter(buffer)
	if err != nil {
		buffer.Flush()
		file.Close()
		return nil, err
	}
	return &FlatWriter{file: file, xz: xzWriter, buffer: buffer}, nil
}

// WriteFrame appends a pre-marshaled sample (the raw bytes from a segment
// frame) with a varint length prefix. Avoids an unmarshal/remarshal round trip.
func (self *FlatWriter) WriteFrame(frame []byte) error {
	n := binary.PutUvarint(self.lenBuf[:], uint64(len(frame)))
	if _, err := self.xz.Write(self.lenBuf[:n]); err != nil {
		return err
	}
	if _, err := self.xz.Write(frame); err != nil {
		return err
	}
	self.count += 1
	return nil
}

func (self *FlatWriter) Count() int { return self.count }

func (self *FlatWriter) Close() error {
	xzErr := self.xz.Close()
	bufErr := self.buffer.Flush()
	fileErr := self.file.Close()
	if xzErr != nil {
		return xzErr
	}
	if bufErr != nil {
		return bufErr
	}
	return fileErr
}

// AppendSegmentTo decompresses one zstd segment and writes each of its frames
// to the flat writer.
func AppendSegmentTo(w *FlatWriter, r io.Reader) error {
	return ReadSegmentStream(r, func(frame []byte) error {
		return w.WriteFrame(frame)
	})
}

// WriteFlatFromSegmentFiles flattens a set of local .pb.zst segment files into
// one xz flat file, returning the frame count.
func WriteFlatFromSegmentFiles(segmentPaths []string, outPath string) (int, error) {
	writer, err := NewFlatWriter(outPath)
	if err != nil {
		return 0, err
	}
	for _, path := range segmentPaths {
		if err := func() error {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			return AppendSegmentTo(writer, file)
		}(); err != nil {
			writer.Close()
			return 0, err
		}
	}
	if err := writer.Close(); err != nil {
		return 0, err
	}
	return writer.Count(), nil
}

// ReadFlat reads an xz varint-delimited flat file, calling onSample for each
// FindProviders2Sample. The sample pointer is reused between calls; copy what
// you need to keep.
func ReadFlat(path string, onSample func(*sample.FindProviders2Sample) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	xzReader, err := xz.NewReader(bufio.NewReaderSize(file, 1<<20))
	if err != nil {
		return err
	}
	reader := bufio.NewReaderSize(xzReader, 1<<20)

	options := protodelim.UnmarshalOptions{}
	for {
		var s sample.FindProviders2Sample
		if err := options.UnmarshalFrom(reader, &s); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := onSample(&s); err != nil {
			return err
		}
	}
}
