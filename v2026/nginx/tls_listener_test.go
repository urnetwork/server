package nginx

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type testHeaderDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (self *testHeaderDeadlineConn) SetReadDeadline(deadline time.Time) error {
	self.deadlines = append(self.deadlines, deadline)
	return nil
}

// Progress across a HEADERS/CONTINUATION pair shares an absolute deadline.
// Its final payload byte releases the timer even with a DATA frame in flight.
func TestH2HeaderDeadlineFollowsFrameBoundaries(t *testing.T) {
	raw := &testHeaderDeadlineConn{}
	conn := &frontTlsConn{Conn: tls.Server(raw, &tls.Config{}), headerTimeout: time.Minute, prefaceLeft: len(http2.ClientPreface)}
	var frames bytes.Buffer
	writer := http2.NewFramer(&frames, nil)
	if err := writer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte{0x82}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteContinuation(1, true, []byte{0x87, 0x84}); err != nil {
		t.Fatal(err)
	}
	headEnd := frames.Len()
	if err := writer.WriteData(1, true, []byte("body")); err != nil {
		t.Fatal(err)
	}
	conn.observeFrames([]byte(http2.ClientPreface))
	if len(raw.deadlines) != 0 {
		t.Fatalf("preface set deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Next(2))
	if len(raw.deadlines) != 1 || raw.deadlines[0].IsZero() {
		t.Fatalf("initial prefix set deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Next(headEnd - 3))
	if len(raw.deadlines) != 1 {
		t.Fatalf("fragment progress changed deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Next(1 + len(conn.frameHeader)))
	if len(raw.deadlines) != 2 || !raw.deadlines[1].IsZero() {
		t.Fatalf("end of headers and start of DATA set deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Bytes())
	if len(raw.deadlines) != 2 {
		t.Fatalf("body changed deadlines %v", raw.deadlines)
	}
}
