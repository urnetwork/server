// H1 server write-batch tests force ready-drain, flush, cancellation, and
// pooled-buffer ownership boundaries without depending on scheduler timing.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// Records the deadline and complete logical WebSocket messages presented by
// the production ready-batch helper.
type connectH1BatchTestWriter struct {
	deadlines  []time.Time
	messages   [][]byte
	writeIndex int
	writeErrAt int
	writeErr   error
}

// Records one deadline operation.
func (self *connectH1BatchTestWriter) SetWriteDeadline(deadline time.Time) error {
	self.deadlines = append(self.deadlines, deadline)
	return nil
}

// Records one copied logical message and optionally injects a frame error.
func (self *connectH1BatchTestWriter) WriteMessage(
	messageType int,
	message []byte,
) error {
	self.writeIndex += 1
	if self.writeErrAt == self.writeIndex {
		return self.writeErr
	}
	self.messages = append(self.messages, bytes.Clone(message))
	return nil
}

// Records explicit batch lifecycle calls and can hold or fail the terminal
// flush at an exact barrier.
type connectH1BatchTestBoundary struct {
	beginCount int
	abortCount int
	flushCount int
	flushErr   error

	flushEntered chan struct{}
	flushRelease chan struct{}
	enteredOnce  sync.Once
}

// Records the start of one batch.
func (self *connectH1BatchTestBoundary) BeginWriteBatch() {
	self.beginCount += 1
}

// Records that retained bytes were discarded.
func (self *connectH1BatchTestBoundary) AbortWriteBatch() {
	self.abortCount += 1
}

// Publishes the exact terminal boundary before returning its injected result.
func (self *connectH1BatchTestBoundary) FlushWriteBatch() error {
	self.flushCount += 1
	if self.flushEntered != nil {
		self.enteredOnce.Do(func() {
			close(self.flushEntered)
		})
	}
	if self.flushRelease != nil {
		<-self.flushRelease
	}
	return self.flushErr
}

// Returns a distinct pooled user message whose first byte identifies its FIFO
// position.
func newConnectH1BatchTestMessage(index byte) []byte {
	message := clientconnect.MessagePoolGet(64)
	for byteIndex := range message {
		message[byteIndex] = index
	}
	return message
}

func TestConnectH1ReadyDrainBalancesAcksAndPayloadBytes(t *testing.T) {
	readyCount := func(messageByteCount int) (count int, byteCount int) {
		count = 1
		byteCount = messageByteCount
		for connectH1WriteBatchCanDrain(count, byteCount) {
			count += 1
			byteCount += messageByteCount
		}
		return
	}

	if count, bytes := readyCount(128); count != 32 || bytes != 4*1024 {
		t.Fatalf("ACK-sized ready drain=(%d, %dB), want (32, 4096B)", count, bytes)
	}
	if count, bytes := readyCount(clientconnect.DefaultMtu); count != 12 ||
		bytes >= 16*1024 {
		t.Fatalf("tunnel-MTU ready drain=(%d, %dB), want 12 below 16KiB", count, bytes)
	}
	if count, bytes := readyCount(4 * 1024); count != 3 ||
		bytes != connectH1WriteBatchDrainByteCount {
		t.Fatalf("maximum H1 ready drain=(%d, %dB), want (3, 12288B)", count, bytes)
	}
}

// One maximum ready batch shares one deadline and flush while the next message
// retains its place in the source queue for the next writer iteration.
func TestConnectH1UserReadyBatchBoundsDrainAndPreservesFifo(t *testing.T) {
	receive := make(chan []byte, connectH1WriteBatchMaxMessageCount)
	messages := make([][]byte, connectH1WriteBatchMaxMessageCount+1)
	witnesses := make([][]byte, connectH1WriteBatchMaxMessageCount)
	for messageIndex := range messages {
		messages[messageIndex] = newConnectH1BatchTestMessage(byte(messageIndex + 1))
		if messageIndex < len(witnesses) {
			witnesses[messageIndex] = clientconnect.MessagePoolShareReadOnly(
				messages[messageIndex],
			)
		}
	}
	for _, message := range messages[1:] {
		receive <- message
	}

	writer := &connectH1BatchTestWriter{}
	boundary := &connectH1BatchTestBoundary{}
	var sentByteCounts []ByteCount
	open, err := writeConnectH1UserReadyBatch(
		context.Background(),
		writer,
		boundary,
		receive,
		messages[0],
		true,
		5*time.Second,
		func(sentByteCount ByteCount) {
			sentByteCounts = append(sentByteCounts, sentByteCount)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !open {
		t.Fatal("ready batch unexpectedly closed its source")
	}
	if len(writer.deadlines) != 1 {
		t.Fatalf("deadline count = %d, want 1", len(writer.deadlines))
	}
	if boundary.beginCount != 1 || boundary.flushCount != 1 || boundary.abortCount != 0 {
		t.Fatalf(
			"batch lifecycle = begin:%d flush:%d abort:%d, want 1/1/0",
			boundary.beginCount,
			boundary.flushCount,
			boundary.abortCount,
		)
	}
	if len(writer.messages) != connectH1WriteBatchMaxMessageCount {
		t.Fatalf(
			"written message count = %d, want %d",
			len(writer.messages),
			connectH1WriteBatchMaxMessageCount,
		)
	}
	if len(sentByteCounts) != len(writer.messages) {
		t.Fatalf(
			"accounted message count = %d, want %d",
			len(sentByteCounts),
			len(writer.messages),
		)
	}
	for messageIndex, message := range writer.messages {
		want := byte(messageIndex + 1)
		if message[0] != want || message[len(message)-1] != want {
			t.Fatalf("message %d lost FIFO identity", messageIndex)
		}
		if !clientconnect.MessagePoolReturn(witnesses[messageIndex]) {
			t.Fatalf("message %d retained pooled ownership", messageIndex)
		}
	}

	select {
	case pendingMessage := <-receive:
		pendingIdentity := pendingMessage[0]
		if pendingIdentity != byte(len(messages)) {
			clientconnect.MessagePoolReturn(pendingMessage)
			t.Fatalf("pending message identity = %d, want %d", pendingIdentity, len(messages))
		}
		clientconnect.MessagePoolReturn(pendingMessage)
	default:
		t.Fatal("bounded batch consumed the next ready message")
	}
}

// An unwrapped connection retains the historical singleton fallback and does
// not dequeue a second ready message.
func TestConnectH1UserReadyBatchUnwrappedFallbackStaysSingleton(t *testing.T) {
	receive := make(chan []byte, 1)
	firstMessage := newConnectH1BatchTestMessage(1)
	secondMessage := newConnectH1BatchTestMessage(2)
	receive <- secondMessage
	writer := &connectH1BatchTestWriter{}

	open, err := writeConnectH1UserReadyBatch(
		context.Background(),
		writer,
		nil,
		receive,
		firstMessage,
		true,
		5*time.Second,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !open || len(writer.messages) != 1 {
		t.Fatalf("fallback result open=%t messages=%d, want true/1", open, len(writer.messages))
	}
	select {
	case pendingMessage := <-receive:
		pendingIdentity := pendingMessage[0]
		if pendingIdentity != 2 {
			clientconnect.MessagePoolReturn(pendingMessage)
			t.Fatalf("pending message identity = %d, want 2", pendingIdentity)
		}
		clientconnect.MessagePoolReturn(pendingMessage)
	default:
		t.Fatal("singleton fallback drained another message")
	}
}

// A failed production type assertion must remain a nil batch interface; a
// typed nil pointer would otherwise enter the batch path and panic.
func TestConnectH1WriteBatchForConnRejectsTypedNil(t *testing.T) {
	var typedNil *clientconnect.WebSocketWriteBatchConn
	if writeBatch := connectH1WriteBatchForConn(typedNil); writeBatch != nil {
		t.Fatalf("typed nil produced a non-nil batch boundary: %T", writeBatch)
	}
	if writeBatch := connectH1WriteBatchForConn(&connectH1BatchTestConn{}); writeBatch != nil {
		t.Fatalf("unwrapped connection produced a batch boundary: %T", writeBatch)
	}
}

// A failed terminal flush publishes no success accounting and releases every
// message the batch dequeued.
func TestConnectH1UserReadyBatchFlushErrorReturnsOwnership(t *testing.T) {
	flushErr := errors.New("injected terminal flush error")
	receive := make(chan []byte, connectH1WriteBatchMaxMessageCount-1)
	messages := make([][]byte, connectH1WriteBatchMaxMessageCount)
	witnesses := make([][]byte, len(messages))
	for messageIndex := range messages {
		messages[messageIndex] = newConnectH1BatchTestMessage(byte(messageIndex + 1))
		witnesses[messageIndex] = clientconnect.MessagePoolShareReadOnly(
			messages[messageIndex],
		)
		if 0 < messageIndex {
			receive <- messages[messageIndex]
		}
	}
	writer := &connectH1BatchTestWriter{}
	boundary := &connectH1BatchTestBoundary{flushErr: flushErr}
	sentCount := 0

	_, err := writeConnectH1UserReadyBatch(
		context.Background(),
		writer,
		boundary,
		receive,
		messages[0],
		true,
		5*time.Second,
		func(ByteCount) {
			sentCount += 1
		},
	)
	if !errors.Is(err, flushErr) {
		t.Fatalf("write error = %v, want %v", err, flushErr)
	}
	if sentCount != 0 {
		t.Fatalf("successful accounting count = %d, want 0", sentCount)
	}
	if boundary.abortCount != 1 {
		t.Fatalf("abort count = %d, want 1", boundary.abortCount)
	}
	for messageIndex, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("failed batch retained message %d", messageIndex)
		}
	}
}

// A logical frame failure aborts the retained socket batch, publishes no
// successful accounting, and returns every message already dequeued from the
// resident route.
func TestConnectH1UserReadyBatchFrameErrorReturnsOwnership(t *testing.T) {
	writeErr := errors.New("injected logical frame error")
	receive := make(chan []byte, connectH1WriteBatchMaxMessageCount-1)
	messages := make([][]byte, connectH1WriteBatchMaxMessageCount)
	witnesses := make([][]byte, len(messages))
	for messageIndex := range messages {
		messages[messageIndex] = newConnectH1BatchTestMessage(byte(messageIndex + 1))
		witnesses[messageIndex] = clientconnect.MessagePoolShareReadOnly(
			messages[messageIndex],
		)
		if 0 < messageIndex {
			receive <- messages[messageIndex]
		}
	}
	writer := &connectH1BatchTestWriter{
		writeErrAt: 2,
		writeErr:   writeErr,
	}
	boundary := &connectH1BatchTestBoundary{}
	sentCount := 0

	_, err := writeConnectH1UserReadyBatch(
		context.Background(),
		writer,
		boundary,
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
		t.Fatalf("successful accounting count = %d, want 0", sentCount)
	}
	if boundary.beginCount != 1 || boundary.flushCount != 0 || boundary.abortCount != 1 {
		t.Fatalf(
			"batch lifecycle = begin:%d flush:%d abort:%d, want 1/0/1",
			boundary.beginCount,
			boundary.flushCount,
			boundary.abortCount,
		)
	}
	for messageIndex, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatalf("failed batch retained message %d", messageIndex)
		}
	}
}

// Handler idle cannot publish while its writer owns a batch at the terminal
// socket flush, and the join becomes the exact ownership-release boundary.
func TestConnectH1WorkersJoinHeldReadyBatchOwnership(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	writerCtx, writerCancel := context.WithCancel(context.Background())
	defer writerCancel()
	message := newConnectH1BatchTestMessage(1)
	witnessBeforeRelease := clientconnect.MessagePoolShareReadOnly(message)
	witnessAfterJoin := clientconnect.MessagePoolShareReadOnly(message)
	boundary := &connectH1BatchTestBoundary{
		flushEntered: make(chan struct{}),
		flushRelease: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseFlush := func() {
		releaseOnce.Do(func() {
			close(boundary.flushRelease)
		})
	}
	defer releaseFlush()
	writer := &connectH1BatchTestWriter{}
	var workers connectHandlerWorkers
	workers.start(func() {
		_, _ = writeConnectH1UserReadyBatch(
			writerCtx,
			writer,
			boundary,
			nil,
			message,
			true,
			5*time.Second,
			nil,
		)
	})
	select {
	case <-boundary.flushEntered:
	case <-testCtx.Done():
		t.Fatal("writer did not reach its terminal flush barrier")
	}

	stopEntered := make(chan struct{})
	finishDone := make(chan struct{})
	go func() {
		finishH1ConnectHandlerWorkers(&workers, func() {
			writerCancel()
			close(stopEntered)
		})
		close(finishDone)
	}()
	select {
	case <-stopEntered:
	case <-testCtx.Done():
		releaseFlush()
		t.Fatal("handler finisher did not stop the connection")
	}
	select {
	case <-finishDone:
		releaseFlush()
		t.Fatal("handler idle published before the held batch released")
	default:
	}
	if clientconnect.MessagePoolReturn(witnessBeforeRelease) {
		releaseFlush()
		t.Fatal("held batch released pooled ownership before its flush")
	}

	releaseFlush()
	select {
	case <-finishDone:
	case <-testCtx.Done():
		t.Fatal("handler finisher did not join the released batch")
	}
	if !clientconnect.MessagePoolReturn(witnessAfterJoin) {
		t.Fatal("handler join retained ready-batch pooled ownership")
	}
}

// A minimal hijackable response writer exposes one known connection to the
// scoped server wrapper.
type connectH1BatchTestResponseWriter struct {
	header http.Header
	conn   *connectH1BatchTestConn
}

// Returns the mutable response header.
func (self *connectH1BatchTestResponseWriter) Header() http.Header {
	return self.header
}

// Records no response body because the test exercises Hijack directly.
func (self *connectH1BatchTestResponseWriter) Write(buffer []byte) (int, error) {
	return len(buffer), nil
}

// Records no status because the test exercises Hijack directly.
func (self *connectH1BatchTestResponseWriter) WriteHeader(statusCode int) {
}

// Returns the known connection and buffers Gorilla will reset after hijack.
func (self *connectH1BatchTestResponseWriter) Hijack() (
	net.Conn,
	*bufio.ReadWriter,
	error,
) {
	return self.conn, bufio.NewReadWriter(
		bufio.NewReader(self.conn),
		bufio.NewWriter(self.conn),
	), nil
}

// Records delegated writes for the pass-through upgrade boundary test.
type connectH1BatchTestConn struct {
	writes [][]byte
}

// Has no readable data.
func (self *connectH1BatchTestConn) Read(buffer []byte) (int, error) {
	return 0, io.EOF
}

// Records a copied delegated write.
func (self *connectH1BatchTestConn) Write(buffer []byte) (int, error) {
	self.writes = append(self.writes, bytes.Clone(buffer))
	return len(buffer), nil
}

// Has no external resource in the in-memory fixture.
func (self *connectH1BatchTestConn) Close() error {
	return nil
}

// Returns no meaningful local address.
func (self *connectH1BatchTestConn) LocalAddr() net.Addr {
	return nil
}

// Returns no meaningful remote address.
func (self *connectH1BatchTestConn) RemoteAddr() net.Addr {
	return nil
}

// Deadlines are inert in the in-memory fixture.
func (self *connectH1BatchTestConn) SetDeadline(deadline time.Time) error {
	return nil
}

// Read deadlines are inert in the in-memory fixture.
func (self *connectH1BatchTestConn) SetReadDeadline(deadline time.Time) error {
	return nil
}

// Write deadlines are inert in the in-memory fixture.
func (self *connectH1BatchTestConn) SetWriteDeadline(deadline time.Time) error {
	return nil
}

// The HTTP upgrade and auth phase remain pass-through until the transport
// writer explicitly begins its first ready batch.
func TestConnectH1BatchResponseWriterPassesThroughUpgradeWrites(t *testing.T) {
	underlying := &connectH1BatchTestConn{}
	responseWriter := &connectH1BatchTestResponseWriter{
		header: make(http.Header),
		conn:   underlying,
	}
	batchResponseWriter := &connectH1BatchResponseWriter{
		ResponseWriter: responseWriter,
	}

	conn, _, err := batchResponseWriter.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.(*clientconnect.WebSocketWriteBatchConn); !ok {
		t.Fatalf("hijacked connection type = %T, want batching connection", conn)
	}
	handshake := []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n")
	if _, err = conn.Write(handshake); err != nil {
		t.Fatal(err)
	}
	if len(underlying.writes) != 1 || !bytes.Equal(underlying.writes[0], handshake) {
		t.Fatal("upgrade write was retained or changed before batching began")
	}
}
