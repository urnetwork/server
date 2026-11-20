package http3

// urnetwork: a request stream's header phase is bounded by
// Server.ReadHeaderTimeout, and its field section is read into a buffer that
// grows with the bytes that arrive (URNETWORK.md). Upstream read the HEADERS
// frame with no deadline into a buffer of its announced length, so a stream
// that announced a length and sent nothing more held its goroutine, that
// buffer and the connection's idle timer for as long as its client acked
// the connection's PINGs.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/quicvarint"

	"github.com/stretchr/testify/require"
)

// the bound on one step of these tests; they never wait it out on success
const urnetworkReadHeaderStepTimeout = 10 * time.Second

// A stream that sends a HEADERS frame's type and announced length, and
// nothing of its field section, is reset with H3_REQUEST_INCOMPLETE once
// ReadHeaderTimeout passes, and its handler never runs.
func TestUrnetworkReadHeaderTimeoutResetsAStreamWithoutItsHeaders(t *testing.T) {
	var handled atomic.Int64
	clientConn, serverConn := newConnPair(t)
	s := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handled.Add(1)
		}),
		ReadHeaderTimeout: 100 * time.Millisecond,
	}
	go s.ServeQUICConn(serverConn)

	str, err := clientConn.OpenStream()
	require.NoError(t, err)
	frameHead := quicvarint.Append(nil, 0x1)
	frameHead = quicvarint.Append(frameHead, 64)
	_, err = str.Write(frameHead)
	require.NoError(t, err)
	str.SetReadDeadline(time.Now().Add(urnetworkReadHeaderStepTimeout))
	_, err = str.Read(make([]byte, 1))
	var streamErr *quic.StreamError
	require.ErrorAs(t, err, &streamErr)
	require.True(t, streamErr.Remote)
	require.Equal(t, quic.StreamErrorCode(ErrCodeRequestIncomplete), streamErr.ErrorCode)
	require.Zero(t, handled.Load())
}

// The header phase's deadline ends with the parsed request: a handler that
// reads its body only once ReadHeaderTimeout has passed since its stream
// began still reads it whole. Its stream began before the handler ran, so
// once the handler has waited out the timeout, the deadline has passed too.
func TestUrnetworkReadHeaderTimeoutLeavesTheBodyToTheHandler(t *testing.T) {
	const readHeaderTimeout = 100 * time.Millisecond
	bodies := make(chan []byte, 1)
	clientConn, serverConn := newConnPair(t)
	s := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-time.After(3 * readHeaderTimeout)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				body = nil
			}
			bodies <- body
		}),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go s.ServeQUICConn(serverConn)

	tr := &Transport{}
	cc := tr.NewClientConn(clientConn)
	ctx, cancel := context.WithTimeout(context.Background(), urnetworkReadHeaderStepTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://localhost/upload", bytes.NewReader([]byte("synthetic body")))
	require.NoError(t, err)
	rsp, err := cc.RoundTrip(req)
	require.NoError(t, err)
	rsp.Body.Close()
	select {
	case body := <-bodies:
		require.Equal(t, []byte("synthetic body"), body)
	case <-time.After(urnetworkReadHeaderStepTimeout):
		t.Fatal("the handler never read its body")
	}
}

// A reader that delivers its bytes, then ends or blocks, and records the
// largest buffer a Read was given.
type urnetworkRecordingReader struct {
	data    []byte
	blocked bool
	largest int
}

// io.Reader
func (self *urnetworkRecordingReader) Read(p []byte) (int, error) {
	self.largest = max(self.largest, len(p))
	if len(self.data) == 0 {
		if self.blocked {
			return 0, errors.New("synthetic deadline")
		}
		return 0, io.EOF
	}
	n := copy(p, self.data)
	self.data = self.data[n:]
	return n, nil
}

// The field section is read into a buffer that grows with the bytes that
// arrive: a frame announcing 64 KiB that sends ten bytes never asks for a
// 64 KiB read. A whole section is returned exactly, and a stream that ends
// or fails first is an error.
func TestUrnetworkReadHeaderBlockGrowsWithTheBytesThatArrive(t *testing.T) {
	const announced = 64 * 1024
	stalled := &urnetworkRecordingReader{data: bytes.Repeat([]byte{0x2}, 10), blocked: true}
	_, err := readHeaderBlock(stalled, announced)
	require.Error(t, err)
	require.LessOrEqual(t, stalled.largest, 4*1024)

	ended := &urnetworkRecordingReader{data: bytes.Repeat([]byte{0x2}, 10)}
	_, err = readHeaderBlock(ended, announced)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	whole := bytes.Repeat([]byte{0x3}, announced)
	complete := &urnetworkRecordingReader{data: append(bytes.Clone(whole), 0x4)}
	headerBlock, err := readHeaderBlock(complete, announced)
	require.NoError(t, err)
	require.Equal(t, whole, headerBlock)
	// the byte after the section is the stream's, not the section's
	require.Equal(t, []byte{0x4}, complete.data)
}
