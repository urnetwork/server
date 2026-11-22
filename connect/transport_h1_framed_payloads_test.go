// The h1 writer's framed path takes its payload index from a pool instead of
// allocating one per write (LBALTOPT2.md R23). These tests hold the writer to
// the framed grammar, its FIFO order and its ownership of the dequeued
// messages, and show the index is cleared, reused and never allocated.
package connect

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
	"unsafe"

	clientconnect "github.com/urnetwork/connect"
)

// A framed writer: it presents WriteMessages, as FramedMessageConn does, and
// records a copy of each batch and the backing array of each index.
type connectH1FramedTestWriter struct {
	batches       [][][]byte
	indexBackings []*[]byte
	writeErr      error
	// copies nothing, so a call allocates only what the writer under test does
	discard bool
}

func (self *connectH1FramedTestWriter) SetWriteDeadline(time.Time) error {
	return nil
}

func (self *connectH1FramedTestWriter) WriteMessage(int, []byte) error {
	return errors.New("the framed path must not write a singleton message")
}

func (self *connectH1FramedTestWriter) WriteMessages(messages [][]byte) error {
	if self.discard {
		return self.writeErr
	}
	self.indexBackings = append(self.indexBackings, unsafe.SliceData(messages))
	batch := [][]byte{}
	for _, message := range messages {
		batch = append(batch, bytes.Clone(message))
	}
	self.batches = append(self.batches, batch)
	return self.writeErr
}

// A pooled message whose bytes all hold identity.
func newConnectH1FramedTestMessage(identity byte, byteCount int) []byte {
	message := clientconnect.MessagePoolGet(byteCount)
	for i := range message {
		message[i] = identity
	}
	return message
}

// Whether every entry of a pooled index is nil. Under the race detector the
// pool may hand out a new index, which is nil too.
func connectH1FramedPayloadsPoolEntryIsClear() bool {
	payloads := connectH1FramedPayloadsPool.Get().(*[connectH1WriteBatchMaxMessageCount][]byte)
	defer connectH1FramedPayloadsPool.Put(payloads)
	for _, payload := range payloads {
		if payload != nil {
			return false
		}
	}
	return true
}

// A ready batch reaches the framed writer in order, without its frames of 16
// bytes or fewer, and every dequeued message returns to the message pool. The
// index the writer saw is cleared when the write returns, and the next write
// reuses it.
func TestConnectH1UserReadyBatchFramedWriterPoolsItsPayloadIndex(t *testing.T) {
	writer := &connectH1FramedTestWriter{}
	receive := newMessageQueue(8)
	identities := []byte{1, 2, 3, 4, 5}
	messages := [][]byte{}
	witnesses := [][]byte{}
	for _, identity := range identities {
		byteCount := 64
		if identity == 3 {
			// a frame the writer never sends: 16 bytes or fewer
			byteCount = 16
		}
		message := newConnectH1FramedTestMessage(identity, byteCount)
		messages = append(messages, message)
		witnesses = append(witnesses, clientconnect.MessagePoolShareReadOnly(message))
	}
	for _, message := range messages[1:] {
		receive.sendForTest(message)
	}
	sentByteCounts := []ByteCount{}
	open, err := writeConnectH1UserReadyBatch(
		context.Background(),
		writer,
		nil,
		receive,
		messages[0],
		true,
		5*time.Second,
		func(byteCount ByteCount) {
			sentByteCounts = append(sentByteCounts, byteCount)
		},
	)
	if err != nil || !open {
		t.Fatalf("write returned open=%t err=%v, want an open writer and no error", open, err)
	}
	if len(writer.batches) != 1 {
		t.Fatalf("the ready batch took %d framed writes, want 1", len(writer.batches))
	}
	gotIdentities := []byte{}
	for _, message := range writer.batches[0] {
		gotIdentities = append(gotIdentities, message[0])
	}
	if !bytes.Equal(gotIdentities, []byte{1, 2, 4, 5}) {
		t.Fatalf("the framed write carried messages %v, want 1, 2, 4 and 5 in order", gotIdentities)
	}
	if len(sentByteCounts) != 4 {
		t.Fatalf("accounted %d sent messages, want 4", len(sentByteCounts))
	}
	for i, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("message %d was not returned to the message pool", identities[i])
		}
	}
	if !connectH1FramedPayloadsPoolEntryIsClear() {
		t.Fatal("a pooled payload index kept references to returned messages")
	}

	second := newConnectH1FramedTestMessage(6, 64)
	if _, err := writeConnectH1UserReadyBatch(
		context.Background(), writer, nil, receive, second, true, 5*time.Second, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(writer.indexBackings) != 2 {
		t.Fatalf("recorded %d framed writes, want 2", len(writer.indexBackings))
	}
	// the race detector's pool drops some puts, so reuse holds only without it
	if !connectInternalRaceEnabled && writer.indexBackings[0] != writer.indexBackings[1] {
		t.Fatal("the second write took a new payload index instead of the pooled one")
	}
}

// A framed write that fails accounts nothing, returns every message, and
// still clears the index it returns to the pool.
func TestConnectH1UserReadyBatchFramedWriteErrorReturnsOwnership(t *testing.T) {
	writeErr := errors.New("injected framed write error")
	writer := &connectH1FramedTestWriter{writeErr: writeErr}
	receive := newMessageQueue(4)
	messages := [][]byte{}
	witnesses := [][]byte{}
	for identity := byte(1); identity <= 3; identity++ {
		message := newConnectH1FramedTestMessage(identity, 64)
		messages = append(messages, message)
		witnesses = append(witnesses, clientconnect.MessagePoolShareReadOnly(message))
	}
	for _, message := range messages[1:] {
		receive.sendForTest(message)
	}
	sentCount := 0
	_, err := writeConnectH1UserReadyBatch(
		context.Background(),
		writer,
		nil,
		receive,
		messages[0],
		true,
		5*time.Second,
		func(ByteCount) {
			sentCount += 1
		},
	)
	if !errors.Is(err, writeErr) {
		t.Fatalf("write error = %v, want %v", err, writeErr)
	}
	if sentCount != 0 {
		t.Fatalf("a failed framed write accounted %d sent messages, want 0", sentCount)
	}
	for i, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("message %d was not returned after the failed write", i+1)
		}
	}
	if !connectH1FramedPayloadsPoolEntryIsClear() {
		t.Fatal("a failed write left references in its pooled payload index")
	}
}

// The framed path allocates nothing per write once the pools are warm. It
// allocated its 768-byte payload index on every write before.
func TestConnectH1UserReadyBatchFramedWriterDoesNotAllocate(t *testing.T) {
	if connectInternalRaceEnabled {
		t.Skip("the race detector's pool drops puts, so pooled objects are reallocated")
	}
	writer := &connectH1FramedTestWriter{discard: true}
	receive := newMessageQueue(4)
	ctx := context.Background()
	write := func() {
		first := clientconnect.MessagePoolGet(64)
		receive.sendForTest(clientconnect.MessagePoolGet(64))
		receive.sendForTest(clientconnect.MessagePoolGet(64))
		if _, err := writeConnectH1UserReadyBatch(ctx, writer, nil, receive, first, true, 5*time.Second, nil); err != nil {
			t.Fatal(err)
		}
	}
	// warm the message and index pools
	for range 8 {
		write()
	}
	if allocs := testing.AllocsPerRun(200, write); allocs != 0 {
		t.Fatalf("a framed ready batch allocated %.1f times a write, want 0", allocs)
	}
}

// The same measurement as a benchmark, for the before and after numbers.
func BenchmarkConnectH1UserReadyBatchFramedWriter(b *testing.B) {
	writer := &connectH1FramedTestWriter{discard: true}
	receive := newMessageQueue(4)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		first := clientconnect.MessagePoolGet(64)
		receive.sendForTest(clientconnect.MessagePoolGet(64))
		receive.sendForTest(clientconnect.MessagePoolGet(64))
		if _, err := writeConnectH1UserReadyBatch(ctx, writer, nil, receive, first, true, 5*time.Second, nil); err != nil {
			b.Fatal(err)
		}
	}
}
