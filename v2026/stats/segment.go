package stats

// segment reading: the inverse of writer.go, shared by tests and the export
// tooling. A segment is a streaming-zstd stream of length-delimited protobuf
// frames (4-byte big-endian length prefix + bytes).

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// ReadSegment decompresses path and calls onFrame for each raw protobuf frame
// in order. onFrame must not retain the slice past the call.
func ReadSegment(path string, onFrame func(frame []byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return ReadSegmentStream(file, onFrame)
}

// ReadSegmentStream is ReadSegment over an already-open reader.
func ReadSegmentStream(r io.Reader, onFrame func(frame []byte) error) error {
	decoder, err := zstd.NewReader(bufio.NewReader(r))
	if err != nil {
		return err
	}
	defer decoder.Close()

	var lenBuf [4]byte
	var frame []byte
	for {
		if _, err := io.ReadFull(decoder, lenBuf[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if cap(frame) < int(n) {
			frame = make([]byte, n)
		} else {
			frame = frame[:n]
		}
		if _, err := io.ReadFull(decoder, frame); err != nil {
			return fmt.Errorf("short frame: %w", err)
		}
		if err := onFrame(frame); err != nil {
			return err
		}
	}
}
