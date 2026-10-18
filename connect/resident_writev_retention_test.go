// Native tcp controls separate pool returns from borrowed write-vector retention.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Embedding the tcp connection preserves net.Buffers' native writev path.
// Write calls distinguish that path from its ordinary net.Conn fallback.
type exchangeWritevObservedTcpConn struct {
	*net.TCPConn
	writes       atomic.Int64
	deadlines    atomic.Int64
	closeAtBatch bool
}

// Counts ordinary writes while preserving the embedded native writev method.
func (self *exchangeWritevObservedTcpConn) Write(p []byte) (int, error) {
	self.writes.Add(1)
	return self.TCPConn.Write(p)
}

// Closes the batch socket only after its deadline succeeds for the immediate-error case.
func (self *exchangeWritevObservedTcpConn) SetWriteDeadline(deadline time.Time) error {
	err := self.TCPConn.SetWriteDeadline(deadline)
	if self.deadlines.Add(1) == 3 && self.closeAtBatch && err == nil {
		// The header and singleton have completed. Close the real socket only
		// after the batch deadline succeeds, so WriteTo itself sees the error.
		_ = self.TCPConn.Close()
	}
	return err
}

// Successful transport writes preserve payloads and release vector references.
func TestExchangeConnectionWritevTransportSuccess(t *testing.T) {
	testExchangeConnectionWritevTerminalBatch(t, ExchangeOpTransport, "success")
}

// Partial transport writes release borrowed pointers after returning pool ownership.
func TestExchangeConnectionWritevTransportPartialTimeout(t *testing.T) {
	testExchangeConnectionWritevTerminalBatch(t, ExchangeOpTransport, "partial_timeout")
}

// Immediate transport failures release every unconsumed vector reference.
func TestExchangeConnectionWritevTransportImmediateClose(t *testing.T) {
	testExchangeConnectionWritevTerminalBatch(t, ExchangeOpTransport, "immediate_close")
}

// Successful forward writes preserve payloads and release vector references.
func TestExchangeConnectionWritevForwardSuccess(t *testing.T) {
	testExchangeConnectionWritevTerminalBatch(t, ExchangeOpForward, "success")
}

// Partial forward writes release borrowed pointers after returning pool ownership.
func TestExchangeConnectionWritevForwardPartialTimeout(t *testing.T) {
	testExchangeConnectionWritevTerminalBatch(t, ExchangeOpForward, "partial_timeout")
}

// Immediate forward failures release every unconsumed vector reference.
func TestExchangeConnectionWritevForwardImmediateClose(t *testing.T) {
	testExchangeConnectionWritevTerminalBatch(t, ExchangeOpForward, "immediate_close")
}

// Exercises the real handshake and native writev, joins both peers, and checks
// all vector slots while the closed connection and shared-root witnesses remain live.
func testExchangeConnectionWritevTerminalBatch(t *testing.T, op ExchangeOp, outcome string) {
	t.Helper()
	testExchangeConnectionWritevTerminalBatchWithFailure(t, op, outcome, nil)
}

// The failure hook is local to one fixture invocation and runs only after the
// first real writer owns its message and the next pair has three pool shares.
func testExchangeConnectionWritevTerminalBatchWithFailure(
	t testing.TB,
	op ExchangeOp,
	outcome string,
	beforeBatchAdmissionForTest func(*exchangeWritevFixtureOwner),
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	owner := newExchangeWritevFixtureOwner(t, cancel)
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	owner.listener = listener

	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = time.Hour
	settings.WriteTimeout = time.Second
	const batchCount = 32
	const payloadSize = 8 * 1024
	const pressureBatchCount = 16
	const pressureDrainFrameCount = 8
	queuedMessageCount := batchCount
	if outcome == "partial_timeout" {
		queuedMessageCount *= pressureBatchCount
	}
	if settings.ExchangeBufferSize != 4096 || settings.ExchangeWriteBatchCount != 256 ||
		settings.ExchangeWriteBatchByteCount != batchCount*payloadSize || settings.FramerSettings.MaxMessageLen != 16*1024 {
		t.Fatal("fixture no longer exercises the resident's production queue and batch bounds")
	}

	owner.peerDone = make(chan struct{})
	peerDrainRequests := make(chan chan error)
	var peerDrainedWireBytes atomic.Int64
	peerReadBatch := make(chan struct{})
	peerRelease := make(chan struct{})
	peerSettings := *settings
	// Keep read-ahead finite and small while the peer deliberately stops
	// partway through the second payload. Framing is unchanged.
	peerSettings.ExchangeReadBufferByteCount = 16
	go func() {
		defer close(owner.peerDone)
		owner.peerErr = func() error {
			peer, err := listener.AcceptTCP()
			if err != nil {
				return err
			}
			if !owner.acceptPeer(peer) {
				return ctx.Err()
			}
			defer peer.Close()
			readBufferSize := 4096
			if outcome == "success" {
				readBufferSize = 64 * 1024
			}
			if err := peer.SetReadBuffer(readBufferSize); err != nil {
				return err
			}
			buffer := NewReceiveOnlyExchangeBuffer(&peerSettings)
			header, err := buffer.ReadHeader(ctx, peer)
			if err != nil {
				return err
			}
			if header.Op != op {
				return fmt.Errorf("handshake op = %v, want %v", header.Op, op)
			}
			if err := buffer.WriteHeader(ctx, peer, header); err != nil {
				return err
			}
			readPayload := func(want []byte) error {
				message, err := buffer.ReadMessage(peer)
				if err != nil {
					return err
				}
				defer clientconnect.MessagePoolReturn(message)
				if !bytes.Equal(message, want) {
					return fmt.Errorf("payload mismatch: got %d bytes, want %d", len(message), len(want))
				}
				return nil
			}
			if err := readPayload([]byte("first")); err != nil {
				return err
			}
			if outcome == "immediate_close" {
				message, err := buffer.ReadMessage(peer)
				clientconnect.MessagePoolReturn(message)
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					return fmt.Errorf("immediate socket close = %v", err)
				}
				return nil
			}
			readCount := batchCount
			if outcome == "partial_timeout" {
				readCount = 1
			}
			for i := range readCount {
				if err := readPayload(bytes.Repeat([]byte{byte(i/2 + 1)}, payloadSize)); err != nil {
					return fmt.Errorf("batch frame %d: %w", i, err)
				}
			}
			if outcome == "partial_timeout" {
				prefix := make([]byte, exchangeIOFrameHeaderByteCount+17)
				if _, err := io.ReadFull(buffer.connReader(peer), prefix); err != nil {
					return err
				}
				if binary.BigEndian.Uint16(prefix[:2]) != payloadSize || binary.BigEndian.Uint16(prefix[2:4]) != 0 ||
					!bytes.Equal(prefix[4:], bytes.Repeat([]byte{1}, 17)) {
					return fmt.Errorf("partial frame prefix did not preserve wire bytes")
				}
			}
			close(peerReadBatch)
			// Later targets drain exactly eight framed messages' worth of bytes
			// from already successful batches. The original partial prefix stays
			// verified above, and every following header/body byte is checked.
			peerWireByteOffset := payloadSize + exchangeIOFrameHeaderByteCount + exchangeIOFrameHeaderByteCount + 17
			var drain []byte
			var frameHeader [exchangeIOFrameHeaderByteCount]byte
			binary.BigEndian.PutUint16(frameHeader[:2], payloadSize)
			for {
				select {
				case result := <-peerDrainRequests:
					if drain == nil {
						drain = make([]byte, pressureDrainFrameCount*(payloadSize+exchangeIOFrameHeaderByteCount))
					}
					err := func() error {
						if _, err := io.ReadFull(buffer.connReader(peer), drain); err != nil {
							return err
						}
						for i, got := range drain {
							offset := peerWireByteOffset + i
							frameIndex := offset / (payloadSize + exchangeIOFrameHeaderByteCount)
							frameOffset := offset % (payloadSize + exchangeIOFrameHeaderByteCount)
							want := byte(frameIndex%batchCount/2 + 1)
							if frameOffset < exchangeIOFrameHeaderByteCount {
								want = frameHeader[frameOffset]
							}
							if got != want {
								return fmt.Errorf("pressure drain wire byte %d = %d, want %d", offset, got, want)
							}
						}
						peerWireByteOffset += len(drain)
						peerDrainedWireBytes.Add(int64(len(drain)))
						return nil
					}()
					result <- err
					if err != nil {
						return err
					}
				case <-peerRelease:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}()
	}()

	var socket *exchangeWritevObservedTcpConn
	settings.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
		if err != nil {
			return nil, err
		}
		tcp := conn.(*net.TCPConn)
		if err := tcp.SetWriteBuffer(4096); err != nil {
			tcp.Close()
			return nil, err
		}
		socket = &exchangeWritevObservedTcpConn{TCPConn: tcp, closeAtBatch: outcome == "immediate_close"}
		return socket, nil
	}
	connection, err := NewExchangeConnection(ctx, ExchangeHeader{
		Version: 1, ClientId: server.NewId(), ResidentId: server.NewId(), Op: op,
	}, "fixture.example", 1, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	owner.connection = connection
	firstDequeued := make(chan struct{})
	releaseFirst := make(chan struct{})
	var dequeuedBatches int
	var writesBeforeBatch int64
	var batchWriteStarted atomic.Bool
	var previousNativeBatchFull atomic.Bool
	var peerDrainErr error
	connection.afterSendDequeueForTest = func() {
		dequeuedBatches++
		if dequeuedBatches == 1 {
			close(firstDequeued)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
			}
		} else if dequeuedBatches == 2 {
			writesBeforeBatch = socket.writes.Load()
			batchWriteStarted.Store(true)
		} else if outcome == "partial_timeout" {
			if !previousNativeBatchFull.Load() {
				peerDrainErr = fmt.Errorf("pressure drain requested without a fully written predecessor")
				connection.Cancel()
				return
			}
			// The single writer owns its next target. Drain only bytes from
			// prior full native batches, then begin its unchanged write deadline.
			result := make(chan error, 1)
			select {
			case peerDrainRequests <- result:
			case <-ctx.Done():
				return
			}
			select {
			case peerDrainErr = <-result:
				if peerDrainErr != nil {
					connection.Cancel()
				}
			case <-ctx.Done():
			}
		}
	}
	before := snapshotExchangeIO("sent", "data")
	// Only immutable scalar/error observations cross the native writer boundary.
	type batchWriteResult struct {
		messageCount   int
		err            error
		elapsed        time.Duration
		connectionDone bool
		parentErr      error
	}
	var batchWriteMessages atomic.Pointer[batchWriteResult]
	batchWriteResults := make(chan *batchWriteResult, pressureBatchCount)
	type nativeWritevResult struct {
		messageCount     int
		writtenByteCount int64
		err              error
	}
	var nativeWritev atomic.Pointer[nativeWritevResult]
	var nativeWritevCalls atomic.Int64
	// Only the writer appends; the test reads the history after owner join.
	nativeHistory := make([]nativeWritevResult, 0, pressureBatchCount)
	connection.sendBuffer.afterWritevForTest = func(messageCount int, writtenByteCount int64, writeErr error) {
		result := nativeWritevResult{
			messageCount:     messageCount,
			writtenByteCount: writtenByteCount,
			err:              writeErr,
		}
		nativeHistory = append(nativeHistory, result)
		nativeWritevCalls.Add(1)
		nativeWritev.Store(&result)
		previousNativeBatchFull.Store(messageCount == batchCount && writtenByteCount == batchCount*(payloadSize+4) && writeErr == nil)
	}
	observationStarted := time.Now()
	connection.afterSendWriteForTest = func(messageCount int, writeErr error) {
		if 1 < messageCount {
			result := &batchWriteResult{
				messageCount:   messageCount,
				err:            writeErr,
				elapsed:        time.Since(observationStarted),
				connectionDone: connection.IsDone(),
				parentErr:      ctx.Err(),
			}
			batchWriteMessages.Store(result)
			nativeResult := nativeWritev.Load()
			nativeWrittenByteCount := int64(-1)
			var nativeWriteErr error
			if nativeResult != nil {
				nativeWrittenByteCount = nativeResult.writtenByteCount
				nativeWriteErr = nativeResult.err
			}
			after := snapshotExchangeIO("sent", "data")
			t.Logf("native_pressure_batch index=%d batch_messages=%d native_bytes=%d native_error=%v write_error=%v complete_frames=%v complete_frame_wire_bytes=%v drained_wire_bytes=%d return_elapsed=%s parent_error=%v",
				nativeWritevCalls.Load(), messageCount, nativeWrittenByteCount, nativeWriteErr, writeErr, after.frames-before.frames, after.bytes-before.bytes, peerDrainedWireBytes.Load(), result.elapsed, result.parentErr)
			select {
			case batchWriteResults <- result:
			case <-ctx.Done():
			}
		}
	}
	logBatchWrite := func(phase string) {
		result := batchWriteMessages.Load()
		nativeResult := nativeWritev.Load()
		nativeMessageCount, nativeWrittenByteCount := 0, int64(-1)
		var nativeWriteErr error
		if nativeResult != nil {
			nativeMessageCount = nativeResult.messageCount
			nativeWrittenByteCount = nativeResult.writtenByteCount
			nativeWriteErr = nativeResult.err
		}
		batchStarted := batchWriteStarted.Load()
		fallbackWrites := int64(-1)
		if batchStarted {
			fallbackWrites = socket.writes.Load() - writesBeforeBatch
		}
		messageCount, elapsed, doneAtReturn := 0, time.Duration(0), false
		var writeErr error
		if result != nil {
			messageCount, writeErr = result.messageCount, result.err
			elapsed, doneAtReturn = result.elapsed, result.connectionDone
		}
		var timeoutError net.Error
		writeTimeout := errors.As(writeErr, &timeoutError) && timeoutError.Timeout()
		peerPrefixReady, ownerJoined := false, false
		select {
		case <-peerReadBatch:
			peerPrefixReady = true
		default:
		}
		select {
		case <-connection.done:
			ownerJoined = true
		default:
		}
		after := snapshotExchangeIO("sent", "data")
		t.Logf("native_write_observation phase=%s batch_started=%t batch_returned=%t batch_messages=%d write_error=%v write_timeout=%t return_elapsed=%s done_at_return=%t complete_frames=%v complete_frame_wire_bytes=%v peer_prefix_ready=%t done=%t owner_joined=%t parent_error=%v deadlines=%d fallback_writes=%d native_writev_calls=%d native_writev_messages=%d native_writev_bytes=%d native_writev_error=%v drained_wire_bytes=%d queued_messages=%d",
			phase, batchStarted, result != nil, messageCount, writeErr, writeTimeout, elapsed, doneAtReturn,
			after.frames-before.frames, after.bytes-before.bytes, peerPrefixReady, connection.IsDone(), ownerJoined, ctx.Err(),
			socket.deadlines.Load(), fallbackWrites, nativeWritevCalls.Load(), nativeMessageCount, nativeWrittenByteCount, nativeWriteErr, peerDrainedWireBytes.Load(), queuedMessageCount)
	}
	first := clientconnect.MessagePoolCopy([]byte("first"))
	firstIndex := owner.keepPending(first)
	owner.witnesses = append(owner.witnesses, retainResidentPoolWitness(first))
	if result := connection.sendMessage(ctx.Done(), owner.takePending(firstIndex), nil, time.Second); result != pooledMessageSendDelivered {
		t.Fatalf("first admission = %v", result)
	}
	select {
	case <-firstDequeued:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Two accepted frames share each root, while a third reference stays in the
	// test. This detects missing or duplicate returns without relying on gc.
	for i := range queuedMessageCount / 2 {
		message := clientconnect.MessagePoolCopy(bytes.Repeat([]byte{byte(i%(batchCount/2) + 1)}, payloadSize))
		messageIndex := owner.keepPending(message)
		if cap(message) != payloadSize+clientconnect.MessagePoolMetaByteCount {
			t.Fatal("expected an exact pooled protocol-frame root")
		}
		shared := clientconnect.MessagePoolShareReadOnly(message)
		sharedIndex := owner.keepPending(shared)
		owner.witnesses = append(owner.witnesses, retainResidentPoolWitness(message))
		if i == 0 && beforeBatchAdmissionForTest != nil {
			beforeBatchAdmissionForTest(owner)
		}
		for _, index := range []int{messageIndex, sharedIndex} {
			if result := connection.sendMessage(ctx.Done(), owner.takePending(index), nil, time.Second); result != pooledMessageSendDelivered {
				t.Fatalf("batch admission = %v", result)
			}
		}
	}
	close(releaseFirst)
	if outcome == "success" {
		select {
		case <-peerReadBatch:
		case <-owner.peerDone:
			t.Fatalf("peer ended before complete batch: %v", owner.peerErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	} else {
		if outcome == "partial_timeout" {
			failedBatch := false
			for range pressureBatchCount {
				select {
				case result := <-batchWriteResults:
					failedBatch = result.err != nil
				case <-ctx.Done():
					logBatchWrite("guard_timeout")
					t.Fatal("bounded native pressure did not produce a terminal write")
				}
				if failedBatch {
					break
				}
			}
			if !failedBatch {
				logBatchWrite("all_pressure_batches_full")
				t.Fatal("bounded native pressure fully wrote every batch; no partial timeout was exercised")
			}
		}
		// The real failed write cancels Run. Done is cancellation; Close below
		// additionally joins socket workers and drains both ownership queues.
		select {
		case <-connection.Done():
		case <-ctx.Done():
			logBatchWrite("guard_timeout")
			t.Fatal("native batch did not terminate")
		}
	}
	connection.Close()
	logBatchWrite("after_terminal_join")
	close(peerRelease)
	select {
	case <-owner.peerDone:
		if owner.peerErr != nil {
			t.Fatal(owner.peerErr)
		}
	case <-ctx.Done():
		t.Fatal("peer did not join")
	}
	if outcome == "partial_timeout" {
		select {
		case <-peerReadBatch:
		default:
			t.Fatal("peer did not consume the verified partial frame prefix")
		}
	}
	wantDequeuedBatches := 2
	var terminalCompletedFrameCount int64
	if outcome == "partial_timeout" {
		wantDequeuedBatches = 1 + len(nativeHistory)
		if result := batchWriteMessages.Load(); result == nil || len(nativeHistory) == 0 {
			t.Errorf("native terminal write result missing: %+v", result)
		} else {
			nativeResult := nativeHistory[len(nativeHistory)-1]
			if frameCount, err := exchangeWritevPartialTimeoutFrameCount(nativeResult.writtenByteCount, payloadSize+4, batchCount, nativeResult.err, result.parentErr); err != nil {
				t.Error(err)
			} else {
				terminalCompletedFrameCount = frameCount
			}
		}
		if len(nativeHistory) == 0 || pressureBatchCount < len(nativeHistory) {
			t.Errorf("native pressure batch count = %d, want 1..%d", len(nativeHistory), pressureBatchCount)
		}
		if peerDrainErr != nil {
			t.Errorf("pressure peer drain failed: %v", peerDrainErr)
		}
		wantDrainedWireBytes := int64(max(0, len(nativeHistory)-1) * pressureDrainFrameCount * (payloadSize + exchangeIOFrameHeaderByteCount))
		if peerDrainedWireBytes.Load() != wantDrainedWireBytes {
			t.Errorf("pressure drained %d wire bytes, want %d", peerDrainedWireBytes.Load(), wantDrainedWireBytes)
		}
	}
	if dequeuedBatches != wantDequeuedBatches || socket.deadlines.Load() != int64(wantDequeuedBatches+1) || socket.writes.Load() != writesBeforeBatch {
		t.Fatalf("expected header, singleton, then bounded native batches: batches=%d deadlines=%d fallback writes=%d",
			dequeuedBatches, socket.deadlines.Load(), socket.writes.Load()-writesBeforeBatch)
	}
	if len(connection.send) != 0 || len(connection.receive) != 0 {
		t.Fatal("joined connection retained queued messages")
	}
	if err := owner.close(); err != nil {
		t.Error(err)
	}
	after := snapshotExchangeIO("sent", "data")
	frames, wireBytes := after.frames-before.frames, after.bytes-before.bytes
	switch outcome {
	case "success":
		requireExchangeIODelta(t, "sent", "data", before, batchCount+1, 9+batchCount*(payloadSize+4))
	case "immediate_close":
		requireExchangeIODelta(t, "sent", "data", before, 1, 9)
	case "partial_timeout":
		completedBatchFrameCount := terminalCompletedFrameCount
		for i, result := range nativeHistory {
			if result.messageCount != batchCount {
				t.Errorf("native batch %d has %d messages, want %d", i, result.messageCount, batchCount)
			}
			if i < len(nativeHistory)-1 {
				if result.err != nil || result.writtenByteCount != batchCount*(payloadSize+4) {
					t.Errorf("pressure predecessor %d was not fully written: %+v", i, result)
				}
				completedBatchFrameCount += result.writtenByteCount / (payloadSize + 4)
			}
		}
		if frames != float64(1+completedBatchFrameCount) || wireBytes != float64(9+completedBatchFrameCount*(payloadSize+4)) {
			t.Errorf("native pressure accounting = %v frames/%v bytes, want %d frames/%d bytes", frames, wireBytes, 1+completedBatchFrameCount, 9+completedBatchFrameCount*(payloadSize+4))
		}
	}
	vector := connection.sendBuffer.writeBuffers
	if cap(vector) < 2*batchCount {
		t.Fatalf("writev backing capacity = %d, want a retained batch index", cap(vector))
	}
	retained := 0
	for _, item := range vector[:cap(vector)] {
		if item != nil {
			retained++
		}
	}
	t.Logf("outcome=%s frames=%v wire_bytes=%v vector_len=%d vector_cap=%d retained_references=%d shared_payload_roots=%d",
		outcome, frames, wireBytes, len(vector), cap(vector), retained, queuedMessageCount/2)
	if retained != 0 {
		t.Errorf("closed ExchangeConnection retains %d borrowed writev references after returning payload ownership", retained)
	}
	// Deliberately retain the completed owner through the backing-array check;
	// reclaiming the whole ExchangeConnection cannot make this assertion pass.
	runtime.KeepAlive(connection)
}
