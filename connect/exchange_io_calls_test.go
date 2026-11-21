// Exchange buffers count their socket calls in exchange_io_calls_total, and
// write in one call a frame that took two: a frame larger than the singleton
// scratch, and the handshake header (LBALTOPT2.md R21).
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/netip"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func exchangeIOCallsForTest(op exchangeIOOp, direction exchangeIODirection) float64 {
	return testutil.ToFloat64(exchangeIOCallsByLabel[op][direction])
}

// A pooled frame of byteCount bytes whose bytes count from identity.
func newExchangeIOCallsTestFrame(identity byte, byteCount int) []byte {
	frame := connectlib.MessagePoolGet(byteCount)
	for i := range frame {
		frame[i] = identity + byte(i)
	}
	return frame
}

// A frame that fits the singleton scratch is one framed write; a larger one,
// to the largest the link carries, is one writev of its header and body
// where the framer wrote it in two. The peer reads each whole, and every
// frame returns to the pool.
func TestExchangeBufferWritesALargeFrameInOneCall(t *testing.T) {
	writer, reader := newExchangeTcpBenchmarkPair(t)
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	sender.setIOOp(ExchangeOpForward)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	receiver.setIOOp(ExchangeOpForward)
	writevFrameCounts := []int{}
	sender.afterWritevForTest = func(messageCount int, writtenByteCount int64, writeErr error) {
		writevFrameCounts = append(writevFrameCounts, messageCount)
		if writeErr != nil {
			t.Errorf("a writev failed: %v", writeErr)
		}
	}
	maxMessageLen := settings.FramerSettings.MaxMessageLen
	for i, byteCount := range []int{
		exchangeSingletonWriteStorageByteCount - exchangeIOFrameHeaderByteCount,
		exchangeSingletonWriteStorageByteCount - exchangeIOFrameHeaderByteCount + 1,
		4096,
		maxMessageLen,
	} {
		frame, pooled := connectlib.MessagePoolGetDetailed(byteCount)
		for j := range frame {
			frame[j] = byte(i + j)
		}
		want := bytes.Clone(frame)
		// a frame past the largest pool class is plain memory, with no owner
		// to witness
		var witness []byte
		if pooled {
			witness = connectlib.MessagePoolShareReadOnly(frame)
		}
		callsBefore := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionSent)
		if err := sender.WriteMessage(writer, frame); err != nil {
			t.Fatal(err)
		}
		if calls := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionSent) - callsBefore; calls != 1 {
			t.Fatalf("a %d-byte frame took %.0f write calls, want 1", byteCount, calls)
		}
		got, err := receiver.ReadMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("the peer read a different %d-byte frame", byteCount)
		}
		connectlib.MessagePoolReturn(got)
		if pooled && !connectlib.MessagePoolReturn(witness) {
			t.Fatalf("the %d-byte frame was not returned to the pool", byteCount)
		}
	}
	// the frames past the scratch, each a writev of its own
	if len(writevFrameCounts) != 3 || writevFrameCounts[0] != 1 || writevFrameCounts[1] != 1 || writevFrameCounts[2] != 1 {
		t.Fatalf("the writevs carried %v frames, want three of one", writevFrameCounts)
	}
}

// The handshake header is one write, and the peer decodes it.
func TestExchangeBufferWritesTheHeaderInOneCall(t *testing.T) {
	writer, reader := newExchangeTcpBenchmarkPair(t)
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	sender.setIOOp(ExchangeOpTransport)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	header := ExchangeHeader{
		Version:    1,
		ClientId:   server.NewId(),
		ResidentId: server.NewId(),
		Op:         ExchangeOpTransport,
		ClientIp:   netip.MustParseAddr("198.51.100.7"),
	}
	callsBefore := exchangeIOCallsForTest(exchangeIOOpTransport, exchangeIODirectionSent)
	if err := sender.WriteHeader(context.Background(), writer, &header); err != nil {
		t.Fatal(err)
	}
	if calls := exchangeIOCallsForTest(exchangeIOOpTransport, exchangeIODirectionSent) - callsBefore; calls != 1 {
		t.Fatalf("the header took %.0f write calls, want 1", calls)
	}
	got, err := receiver.ReadHeader(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if *got != header {
		t.Fatalf("the peer decoded %+v, want %+v", *got, header)
	}
}

// Each singleton write is one call, and a batch of frames is one writev.
func TestExchangeBufferCountsItsWriteCalls(t *testing.T) {
	writer, reader := newExchangeTcpBenchmarkPair(t)
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	sender.setIOOp(ExchangeOpForward)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	receiver.setIOOp(ExchangeOpForward)
	callsBefore := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionSent)
	for i := range 3 {
		if err := sender.WriteMessage(writer, newExchangeIOCallsTestFrame(byte(i), 100)); err != nil {
			t.Fatal(err)
		}
	}
	if calls := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionSent) - callsBefore; calls != 3 {
		t.Fatalf("three singleton writes took %.0f calls, want 3", calls)
	}
	batch := [][]byte{}
	for i := range 4 {
		batch = append(batch, newExchangeIOCallsTestFrame(byte(10+i), 1380))
	}
	if err := sender.WriteMessages(writer, batch); err != nil {
		t.Fatal(err)
	}
	if calls := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionSent) - callsBefore; calls != 4 {
		t.Fatalf("a batch of four took %.0f calls after the singletons, want 1", calls-3)
	}
	for range 7 {
		frame, err := receiver.ReadMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		connectlib.MessagePoolReturn(frame)
	}
}

// A link's buffered reader counts each socket read: frames that arrived
// together take one read.
func TestExchangeBufferCountsItsReadCalls(t *testing.T) {
	settings := DefaultExchangeSettings()
	frameBytes := []byte{}
	for i := range 5 {
		header := [exchangeIOFrameHeaderByteCount]byte{}
		binary.BigEndian.PutUint16(header[0:2], 200)
		frameBytes = append(frameBytes, header[:]...)
		frameBytes = append(frameBytes, bytes.Repeat([]byte{byte(i)}, 200)...)
	}
	// returns every frame in its first read, and fails any second one
	conn := &exchangeTcpBenchmarkSingleReadConn{frameBytes: frameBytes}
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	receiver.setIOOp(ExchangeOpForward)
	callsBefore := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionReceived)
	for i := range 5 {
		frame, err := receiver.ReadMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) != 200 || frame[0] != byte(i) {
			t.Fatalf("read frame %d wrongly", i)
		}
		connectlib.MessagePoolReturn(frame)
	}
	if calls := exchangeIOCallsForTest(exchangeIOOpForward, exchangeIODirectionReceived) - callsBefore; calls != 1 {
		t.Fatalf("five frames that arrived together took %.0f read calls, want 1", calls)
	}
}

// A batch writev allocates nothing once the pools are warm: its copy of the
// iovecs escaped to the heap on every batch before.
func TestExchangeBufferWritevAllocatesNothing(t *testing.T) {
	if connectInternalRaceEnabled {
		t.Skip("the race detector's pool drops puts, so pooled buffers are reallocated")
	}
	writer, reader := newExchangeTcpBenchmarkPair(t)
	// discards what arrives, with its one buffer taken before the count
	go io.Copy(io.Discard, reader)
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	sender.setIOOp(ExchangeOpForward)
	batch := make([][]byte, 2)
	write := func() {
		batch[0] = connectlib.MessagePoolGet(100)
		batch[1] = connectlib.MessagePoolGet(100)
		if err := sender.WriteMessages(writer, batch); err != nil {
			t.Fatal(err)
		}
	}
	for range 16 {
		write()
	}
	if allocs := testing.AllocsPerRun(500, write); allocs != 0 {
		t.Fatalf("a batch writev allocated %.2f times, want 0", allocs)
	}
}

// The same measurement as a benchmark, for the before and after numbers.
func BenchmarkExchangeBufferWritevBatch(b *testing.B) {
	writer, reader := newExchangeTcpBenchmarkPair(b)
	go io.Copy(io.Discard, reader)
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	sender.setIOOp(ExchangeOpForward)
	batch := make([][]byte, 2)
	b.ReportAllocs()
	for b.Loop() {
		batch[0] = connectlib.MessagePoolGet(100)
		batch[1] = connectlib.MessagePoolGet(100)
		if err := sender.WriteMessages(writer, batch); err != nil {
			b.Fatal(err)
		}
	}
}
