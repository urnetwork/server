// Deadline rejection is observed at the production exchange framer boundary,
// without sockets, discovery, database state, or a timing-dependent stall.
package connect

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Buffered I/O would succeed if a rejected deadline were ignored. The exact
// sentinel and counters discriminate rejection from downstream framing errors.
type exchangeDeadlineConn struct {
	bytes.Buffer
	readErr    error
	writeErr   error
	readCount  int
	writeCount int
}

// Counts actual reads, including bufio read-ahead on the healthy path.
func (self *exchangeDeadlineConn) Read(message []byte) (int, error) {
	self.readCount++
	return self.Buffer.Read(message)
}

// Counts I/O that must not occur after deadline rejection.
func (self *exchangeDeadlineConn) Write(message []byte) (int, error) {
	self.writeCount++
	return self.Buffer.Write(message)
}

// No external resources are owned by this synthetic connection.
func (self *exchangeDeadlineConn) Close() error { return nil }

// No live address is needed by the exchange framing boundary.
func (self *exchangeDeadlineConn) LocalAddr() net.Addr { return nil }

// No live peer identity is needed by the exchange framing boundary.
func (self *exchangeDeadlineConn) RemoteAddr() net.Addr { return nil }

// The combined setter preserves the first failure; owners use one direction.
func (self *exchangeDeadlineConn) SetDeadline(time.Time) error {
	if self.readErr != nil {
		return self.readErr
	}
	return self.writeErr
}

// A failing installation leaves available bytes intact for the RED control.
func (self *exchangeDeadlineConn) SetReadDeadline(time.Time) error { return self.readErr }

// A failing installation still has a writable buffer for the RED control.
func (self *exchangeDeadlineConn) SetWriteDeadline(time.Time) error { return self.writeErr }

// Authenticated connection setup must not emit a header without its bound.
func TestExchangeDeadlineWriteHeaderRejectedBeforeIo(t *testing.T) {
	conn := &exchangeDeadlineConn{writeErr: errors.New("synthetic header deadline rejected")}
	buffer := NewDefaultExchangeBuffer(DefaultExchangeSettings())
	before := snapshotExchangeIO("sent", "handshake")
	err := buffer.WriteHeader(context.Background(), conn, &ExchangeHeader{Version: 1, Op: ExchangeOpTransport})
	if !errors.Is(err, conn.writeErr) || conn.writeCount != 0 {
		t.Errorf("header deadline result=%v writes=%d", err, conn.writeCount)
	}
	requireExchangeIODelta(t, "sent", "handshake", before, 0, 0)
}

// Valid encoded input distinguishes a rejected read deadline from bad framing.
func TestExchangeDeadlineReadHeaderRejectedBeforeIo(t *testing.T) {
	settings := DefaultExchangeSettings()
	conn := &exchangeDeadlineConn{}
	if err := NewDefaultExchangeBuffer(settings).WriteHeader(context.Background(), conn, &ExchangeHeader{Version: 1, Op: ExchangeOpTransport}); err != nil {
		t.Fatal(err)
	}
	conn.readErr = errors.New("synthetic header read deadline rejected")
	before := snapshotExchangeIO("received", "handshake")
	header, err := NewReceiveOnlyExchangeBuffer(settings).ReadHeader(context.Background(), conn)
	if !errors.Is(err, conn.readErr) || header != nil || conn.readCount != 0 {
		t.Errorf("header deadline result=%v returned_header=%t reads=%d", err, header != nil, conn.readCount)
	}
	requireExchangeIODelta(t, "received", "handshake", before, 0, 0)
}

// The singleton API takes its input on every return, including setter error.
func TestExchangeDeadlineWriteMessageReturnsOwner(t *testing.T) {
	conn := &exchangeDeadlineConn{writeErr: errors.New("synthetic singleton deadline rejected")}
	message := clientconnect.MessagePoolGet(37)
	witness := clientconnect.MessagePoolShareReadOnly(message)
	before := snapshotExchangeIO("sent", "data")
	err := NewDefaultExchangeBuffer(DefaultExchangeSettings()).WriteMessage(conn, message)
	if !clientconnect.MessagePoolReturn(witness) {
		t.Error("deadline rejection retained singleton ownership")
	}
	if !errors.Is(err, conn.writeErr) || conn.writeCount != 0 {
		t.Errorf("singleton deadline result=%v writes=%d", err, conn.writeCount)
	}
	requireExchangeIODelta(t, "sent", "data", before, 0, 0)
}

// A batch rejection must release every gathered input exactly once.
func TestExchangeDeadlineWriteMessagesReturnsOwners(t *testing.T) {
	conn := &exchangeDeadlineConn{writeErr: errors.New("synthetic batch deadline rejected")}
	messages := [][]byte{clientconnect.MessagePoolGet(37), clientconnect.MessagePoolGet(53)}
	witnesses := [][]byte{clientconnect.MessagePoolShareReadOnly(messages[0]), clientconnect.MessagePoolShareReadOnly(messages[1])}
	before := snapshotExchangeIO("sent", "data")
	err := NewDefaultExchangeBuffer(DefaultExchangeSettings()).WriteMessages(conn, messages)
	for _, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Error("deadline rejection retained a gathered message owner")
		}
	}
	if !errors.Is(err, conn.writeErr) || conn.writeCount != 0 {
		t.Errorf("batch deadline result=%v writes=%d", err, conn.writeCount)
	}
	requireExchangeIODelta(t, "sent", "data", before, 0, 0)
}

// A valid complete frame must not bypass the rejected reader bound.
func TestExchangeDeadlineReadMessageRejectedBeforeIo(t *testing.T) {
	settings := DefaultExchangeSettings()
	conn := &exchangeDeadlineConn{}
	if err := NewDefaultExchangeBuffer(settings).WriteMessage(conn, clientconnect.MessagePoolGet(37)); err != nil {
		t.Fatal(err)
	}
	conn.readErr = errors.New("synthetic message read deadline rejected")
	before := snapshotExchangeIO("received", "data")
	message, err := NewReceiveOnlyExchangeBuffer(settings).ReadMessage(conn)
	clientconnect.MessagePoolReturn(message)
	if !errors.Is(err, conn.readErr) || message != nil || conn.readCount != 0 {
		t.Errorf("message deadline result=%v returned_message=%t reads=%d", err, message != nil, conn.readCount)
	}
	requireExchangeIODelta(t, "received", "data", before, 0, 0)
}

// A cached second frame is also subject to the owner's next operation bound.
func TestExchangeDeadlineBufferedFrameCannotHideRejection(t *testing.T) {
	settings := DefaultExchangeSettings()
	conn := &exchangeDeadlineConn{}
	if err := NewDefaultExchangeBuffer(settings).WriteMessages(conn, [][]byte{clientconnect.MessagePoolGet(37), clientconnect.MessagePoolGet(53)}); err != nil {
		t.Fatal(err)
	}
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	message, err := receiver.ReadMessage(conn)
	clientconnect.MessagePoolReturn(message)
	if err != nil {
		t.Fatal(err)
	}
	if receiver.reader.Buffered() == 0 {
		t.Fatal("fixture did not retain the second framed message")
	}
	conn.readErr = errors.New("synthetic cached-frame deadline rejected")
	before := snapshotExchangeIO("received", "data")
	message, err = receiver.ReadMessage(conn)
	clientconnect.MessagePoolReturn(message)
	if !errors.Is(err, conn.readErr) || message != nil {
		t.Errorf("cached frame escaped rejected deadline: %v", err)
	}
	requireExchangeIODelta(t, "received", "data", before, 0, 0)
}

// Successful header framing and decoding stays unchanged.
func TestExchangeDeadlineHealthyHeaderRoundTrip(t *testing.T) {
	settings := DefaultExchangeSettings()
	conn := &exchangeDeadlineConn{}
	header := &ExchangeHeader{Version: 1, Op: ExchangeOpTransport}
	if err := NewDefaultExchangeBuffer(settings).WriteHeader(context.Background(), conn, header); err != nil {
		t.Fatal(err)
	}
	got, err := NewReceiveOnlyExchangeBuffer(settings).ReadHeader(context.Background(), conn)
	if err != nil || got == nil || *got != *header {
		t.Fatalf("healthy header round trip: %v", err)
	}
}

// Nil-deadline-error batches retain all framing and message ownership.
func TestExchangeDeadlineHealthyBatchRoundTrip(t *testing.T) {
	settings := DefaultExchangeSettings()
	conn := &exchangeDeadlineConn{}
	want := [][]byte{bytes.Repeat([]byte{1}, 37), bytes.Repeat([]byte{2}, 53)}
	messages := [][]byte{clientconnect.MessagePoolCopy(want[0]), clientconnect.MessagePoolCopy(want[1])}
	if err := NewDefaultExchangeBuffer(settings).WriteMessages(conn, messages); err != nil {
		t.Fatal(err)
	}
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	for _, expected := range want {
		message, err := receiver.ReadMessage(conn)
		matches := bytes.Equal(message, expected)
		clientconnect.MessagePoolReturn(message)
		if err != nil || !matches {
			t.Fatalf("healthy batch round trip: %v", err)
		}
	}
}

// Empty batches do not install deadlines or manufacture a failed operation.
func TestExchangeDeadlineEmptyBatchDoesNotPerformIo(t *testing.T) {
	conn := &exchangeDeadlineConn{writeErr: errors.New("unused synthetic deadline")}
	if err := NewDefaultExchangeBuffer(DefaultExchangeSettings()).WriteMessages(conn, nil); err != nil || conn.writeCount != 0 {
		t.Fatalf("empty batch performed work: %v", err)
	}
}

// Existing oversized-frame refusal still releases all owners and writes none.
func TestExchangeDeadlineOversizePreservesValidation(t *testing.T) {
	settings := DefaultExchangeSettings()
	settings.FramerSettings.MaxMessageLen = 16
	conn := &exchangeDeadlineConn{}
	messages := [][]byte{clientconnect.MessagePoolGet(8), clientconnect.MessagePoolGet(17)}
	witnesses := [][]byte{clientconnect.MessagePoolShareReadOnly(messages[0]), clientconnect.MessagePoolShareReadOnly(messages[1])}
	err := NewDefaultExchangeBuffer(settings).WriteMessages(conn, messages)
	for _, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Error("validation retained an input owner")
		}
	}
	if err == nil || conn.writeCount != 0 {
		t.Fatalf("oversized batch admitted: %v", err)
	}
}
