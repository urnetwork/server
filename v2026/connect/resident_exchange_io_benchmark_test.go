// Real-loopback benchmarks for the internal exchange TCP boundary. The write
// pair isolates repeated framed writes from the production ready-batch writev
// shape. The dispatch pair keeps the same batched sender and framing work while
// varying only the decoded-message channel handoff.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	connectlib "github.com/urnetwork/connect/v2026"
)

const (
	exchangeTcpBenchmarkMessageByteCount  = 1380
	exchangeTcpBenchmarkBurstMessageCount = 64
)

// Holds one fixed-size decoded burst. The batch benchmark passes a pointer from
// a bounded free list so the handoff does not copy 64 slice headers or allocate
// a descriptor per burst.
type exchangeTcpBenchmarkDispatchBatch struct {
	messageCount int
	messages     [exchangeTcpBenchmarkBurstMessageCount][]byte
}

// A one-frame connection that rejects a second socket read. It deterministically
// proves the ready-only drain does not wait on the socket to fill a batch.
type exchangeTcpBenchmarkSingleReadConn struct {
	frameBytes []byte
	readCount  int
}

// Supplies the complete first frame and rejects any attempt to fetch another.
func (self *exchangeTcpBenchmarkSingleReadConn) Read(buffer []byte) (int, error) {
	self.readCount += 1
	if 1 < self.readCount {
		return 0, errors.New("ready-only batch attempted a second socket read")
	}
	return copy(buffer, self.frameBytes), nil
}

// Rejects writes because this test connection is receive-only.
func (self *exchangeTcpBenchmarkSingleReadConn) Write([]byte) (int, error) {
	return 0, errors.New("receive-only test connection")
}

// Has no external resources.
func (self *exchangeTcpBenchmarkSingleReadConn) Close() error {
	return nil
}

// Returns a synthetic loopback address.
func (self *exchangeTcpBenchmarkSingleReadConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

// Returns a synthetic loopback address.
func (self *exchangeTcpBenchmarkSingleReadConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

// Deadlines are inert because reads are deterministic in memory.
func (self *exchangeTcpBenchmarkSingleReadConn) SetDeadline(time.Time) error {
	return nil
}

// Deadlines are inert because reads are deterministic in memory.
func (self *exchangeTcpBenchmarkSingleReadConn) SetReadDeadline(time.Time) error {
	return nil
}

// Deadlines are inert because writes are rejected immediately.
func (self *exchangeTcpBenchmarkSingleReadConn) SetWriteDeadline(time.Time) error {
	return nil
}

// Creates a real loopback TCP pair so net.Buffers can retain its TCP writev
// implementation instead of falling back through a test connection wrapper.
func newExchangeTcpBenchmarkPair(benchmark *testing.B) (*net.TCPConn, *net.TCPConn) {
	benchmark.Helper()

	listener, err := net.ListenTCP(
		"tcp4",
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
	)
	if err != nil {
		benchmark.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan *net.TCPConn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()

	writer, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		benchmark.Fatal(err)
	}
	var reader *net.TCPConn
	select {
	case err = <-acceptErrors:
		writer.Close()
		benchmark.Fatal(err)
	case reader = <-accepted:
	}

	if err = writer.SetNoDelay(true); err != nil {
		writer.Close()
		reader.Close()
		benchmark.Fatal(err)
	}
	if err = reader.SetNoDelay(true); err != nil {
		writer.Close()
		reader.Close()
		benchmark.Fatal(err)
	}
	benchmark.Cleanup(func() {
		writer.Close()
		reader.Close()
	})
	return writer, reader
}

// Returns one production-sized pooled message. ExchangeBuffer takes ownership
// and returns it after every write result.
func newExchangeTcpBenchmarkMessage(index int) []byte {
	message := connectlib.MessagePoolGet(exchangeTcpBenchmarkMessageByteCount)
	message[0] = byte(index)
	message[len(message)-1] = byte(index >> 8)
	return message
}

// Validates the fixed payload size shared by every benchmark variant.
func validateExchangeTcpBenchmarkMessage(message []byte) error {
	if len(message) != exchangeTcpBenchmarkMessageByteCount {
		return fmt.Errorf(
			"exchange message byte count = %d, want %d",
			len(message),
			exchangeTcpBenchmarkMessageByteCount,
		)
	}
	return nil
}

// Reads and returns exactly one expected message, validating framing while the
// benchmark timer is active.
func readExchangeTcpBenchmarkMessage(
	receiveBuffer *ExchangeBuffer,
	reader *net.TCPConn,
) error {
	message, err := receiveBuffer.ReadMessage(reader)
	if err != nil {
		return err
	}
	if err = validateExchangeTcpBenchmarkMessage(message); err != nil {
		connectlib.MessagePoolReturn(message)
		return err
	}
	connectlib.MessagePoolReturn(message)
	return nil
}

// Reads one blocking frame, then drains only complete frames already held by
// the existing buffered reader. It never performs another socket read merely
// to fill the batch; a later frame begins the next call instead.
func readExchangeTcpBenchmarkReadyBatch(
	receiveBuffer *ExchangeBuffer,
	reader net.Conn,
	batch *exchangeTcpBenchmarkDispatchBatch,
	maximumMessageCount int,
) error {
	batch.messageCount = 0
	bufferedReader := receiveBuffer.connReader(reader)
	for batch.messageCount < min(maximumMessageCount, len(batch.messages)) {
		if 0 < batch.messageCount {
			if bufferedReader.Buffered() < 4 {
				break
			}
			header, err := bufferedReader.Peek(4)
			if err != nil {
				break
			}
			frameByteCount := int(binary.BigEndian.Uint16(header[0:2])) + 4
			if receiveBuffer.settings.FramerSettings.MaxMessageLen+4 < frameByteCount {
				// Let the framer consume and report the invalid ready header.
			} else if bufferedReader.Buffered() < frameByteCount {
				break
			}
		}

		message, err := receiveBuffer.ReadMessage(reader)
		if err != nil {
			for messageIndex := range batch.messageCount {
				connectlib.MessagePoolReturn(batch.messages[messageIndex])
				batch.messages[messageIndex] = nil
			}
			batch.messageCount = 0
			return err
		}
		if err = validateExchangeTcpBenchmarkMessage(message); err != nil {
			connectlib.MessagePoolReturn(message)
			for messageIndex := range batch.messageCount {
				connectlib.MessagePoolReturn(batch.messages[messageIndex])
				batch.messages[messageIndex] = nil
			}
			batch.messageCount = 0
			return err
		}
		batch.messages[batch.messageCount] = message
		batch.messageCount += 1
	}
	return nil
}

// Proves a sparse frame is dispatched without waiting for a second socket read.
func TestExchangeTcpReadyBatchDoesNotWaitToFill(t *testing.T) {
	settings := DefaultExchangeSettings()
	framer := connectlib.NewFramer(settings.FramerSettings)
	message := make([]byte, exchangeTcpBenchmarkMessageByteCount)
	var wire bytes.Buffer
	if err := framer.Write(&wire, message); err != nil {
		t.Fatal(err)
	}

	reader := &exchangeTcpBenchmarkSingleReadConn{frameBytes: wire.Bytes()}
	receiveBuffer := NewReceiveOnlyExchangeBuffer(settings)
	batch := &exchangeTcpBenchmarkDispatchBatch{}
	if err := readExchangeTcpBenchmarkReadyBatch(
		receiveBuffer,
		reader,
		batch,
		exchangeTcpBenchmarkBurstMessageCount,
	); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, received := range batch.messages[:batch.messageCount] {
			connectlib.MessagePoolReturn(received)
		}
	}()

	if batch.messageCount != 1 {
		t.Fatalf("ready message count = %d, want 1", batch.messageCount)
	}
	if reader.readCount != 1 {
		t.Fatalf("socket read count = %d, want 1", reader.readCount)
	}
}

// Measures one 64-message burst as repeated production framed writes or as the
// existing ready-drained writev. Both paths use the same TCP pair and reader.
func runExchangeTcpWriteBenchmark(benchmark *testing.B, batch bool) {
	settings := DefaultExchangeSettings()
	writer, reader := newExchangeTcpBenchmarkPair(benchmark)
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewReceiveOnlyExchangeBuffer(settings)

	totalMessageCount := benchmark.N * exchangeTcpBenchmarkBurstMessageCount
	readResult := make(chan error, 1)
	go func() {
		for range totalMessageCount {
			if err := readExchangeTcpBenchmarkMessage(receiveBuffer, reader); err != nil {
				readResult <- err
				return
			}
		}
		readResult <- nil
	}()

	benchmark.ReportAllocs()
	benchmark.SetBytes(
		int64(exchangeTcpBenchmarkBurstMessageCount * exchangeTcpBenchmarkMessageByteCount),
	)
	benchmark.ResetTimer()

	var writeErr error
	for burstIndex := range benchmark.N {
		if batch {
			var messages [exchangeTcpBenchmarkBurstMessageCount][]byte
			for messageIndex := range messages {
				messages[messageIndex] = newExchangeTcpBenchmarkMessage(
					burstIndex*exchangeTcpBenchmarkBurstMessageCount + messageIndex,
				)
			}
			writeErr = sendBuffer.WriteMessages(writer, messages[:])
		} else {
			for messageIndex := range exchangeTcpBenchmarkBurstMessageCount {
				message := newExchangeTcpBenchmarkMessage(
					burstIndex*exchangeTcpBenchmarkBurstMessageCount + messageIndex,
				)
				if writeErr = sendBuffer.WriteMessage(writer, message); writeErr != nil {
					break
				}
			}
		}
		if writeErr != nil {
			break
		}
	}
	if writeErr != nil {
		writer.Close()
	}
	readErr := <-readResult
	benchmark.StopTimer()

	if writeErr != nil {
		benchmark.Fatal(writeErr)
	}
	if readErr != nil {
		benchmark.Fatal(readErr)
	}
}

// Measures repeated ExchangeBuffer.WriteMessage calls over a real TCP socket.
func BenchmarkExchangeTcpWriteSingletonFrames(benchmark *testing.B) {
	runExchangeTcpWriteBenchmark(benchmark, false)
}

// Measures the production ExchangeBuffer.WriteMessages writev over real TCP.
func BenchmarkExchangeTcpWriteReadyBatch(benchmark *testing.B) {
	runExchangeTcpWriteBenchmark(benchmark, true)
}

// Measures a fixed 64-message burst split into eight-message writev calls.
func BenchmarkExchangeTcpWriteReadyBatch8(benchmark *testing.B) {
	runExchangeTcpWriteDepthBenchmark(benchmark, 8)
}

// Measures a fixed 64-message burst in one writev call, matching the effective
// current production bound for this ready backlog.
func BenchmarkExchangeTcpWriteReadyBatch64(benchmark *testing.B) {
	runExchangeTcpWriteDepthBenchmark(
		benchmark,
		exchangeTcpBenchmarkBurstMessageCount,
	)
}

// Measures a fixed ready backlog while varying only the maximum messages in
// each ExchangeBuffer.WriteMessages call.
func runExchangeTcpWriteDepthBenchmark(
	benchmark *testing.B,
	maximumMessageCount int,
) {
	settings := DefaultExchangeSettings()
	writer, reader := newExchangeTcpBenchmarkPair(benchmark)
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewReceiveOnlyExchangeBuffer(settings)

	totalMessageCount := benchmark.N * exchangeTcpBenchmarkBurstMessageCount
	readResult := make(chan error, 1)
	go func() {
		for range totalMessageCount {
			if err := readExchangeTcpBenchmarkMessage(receiveBuffer, reader); err != nil {
				readResult <- err
				return
			}
		}
		readResult <- nil
	}()

	benchmark.ReportAllocs()
	benchmark.SetBytes(
		int64(exchangeTcpBenchmarkBurstMessageCount * exchangeTcpBenchmarkMessageByteCount),
	)
	benchmark.ResetTimer()

	writeCallCount := 0
	var writeErr error
	for burstIndex := range benchmark.N {
		var messages [exchangeTcpBenchmarkBurstMessageCount][]byte
		for messageIndex := range messages {
			messages[messageIndex] = newExchangeTcpBenchmarkMessage(
				burstIndex*exchangeTcpBenchmarkBurstMessageCount + messageIndex,
			)
		}
		for firstMessageIndex := 0; firstMessageIndex < len(messages); firstMessageIndex += maximumMessageCount {
			lastMessageIndex := min(
				firstMessageIndex+maximumMessageCount,
				len(messages),
			)
			writeCallCount += 1
			writeErr = sendBuffer.WriteMessages(
				writer,
				messages[firstMessageIndex:lastMessageIndex],
			)
			if writeErr != nil {
				for _, message := range messages[lastMessageIndex:] {
					connectlib.MessagePoolReturn(message)
				}
				break
			}
		}
		if writeErr != nil {
			break
		}
	}
	if writeErr != nil {
		writer.Close()
	}
	readErr := <-readResult
	benchmark.StopTimer()

	if writeErr != nil {
		benchmark.Fatal(writeErr)
	}
	if readErr != nil {
		benchmark.Fatal(readErr)
	}
	benchmark.ReportMetric(
		float64(totalMessageCount)/float64(writeCallCount),
		"messages/write",
	)
}

// Measures the deterministic outbound resident shape: the first message wakes
// the connection writer, then the already-bridged remainder forms one writev.
func BenchmarkExchangeTcpWriteBridgeFirstSingleton(benchmark *testing.B) {
	settings := DefaultExchangeSettings()
	writer, reader := newExchangeTcpBenchmarkPair(benchmark)
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewReceiveOnlyExchangeBuffer(settings)
	totalMessageCount := benchmark.N * exchangeTcpBenchmarkBurstMessageCount
	readResult := make(chan error, 1)
	go func() {
		for range totalMessageCount {
			if err := readExchangeTcpBenchmarkMessage(receiveBuffer, reader); err != nil {
				readResult <- err
				return
			}
		}
		readResult <- nil
	}()

	benchmark.ReportAllocs()
	benchmark.SetBytes(int64(exchangeTcpBenchmarkBurstMessageCount * exchangeTcpBenchmarkMessageByteCount))
	benchmark.ResetTimer()
	var writeErr error
	for burstIndex := range benchmark.N {
		var messages [exchangeTcpBenchmarkBurstMessageCount][]byte
		for messageIndex := range messages {
			messages[messageIndex] = newExchangeTcpBenchmarkMessage(
				burstIndex*exchangeTcpBenchmarkBurstMessageCount + messageIndex,
			)
		}
		if writeErr = sendBuffer.WriteMessage(writer, messages[0]); writeErr != nil {
			for _, message := range messages[1:] {
				connectlib.MessagePoolReturn(message)
			}
			break
		}
		if writeErr = sendBuffer.WriteMessages(writer, messages[1:]); writeErr != nil {
			break
		}
	}
	if writeErr != nil {
		writer.Close()
	}
	readErr := <-readResult
	benchmark.StopTimer()
	if writeErr != nil {
		benchmark.Fatal(writeErr)
	}
	if readErr != nil {
		benchmark.Fatal(readErr)
	}
}

// Measures the same batched sender and framed TCP reader while varying only
// whether decoded messages cross the bounded dispatch boundary one at a time
// or in reusable 64-message descriptors.
func runExchangeTcpDispatchBenchmark(
	benchmark *testing.B,
	maximumBatchMessageCount int,
) {
	settings := DefaultExchangeSettings()
	writer, reader := newExchangeTcpBenchmarkPair(benchmark)
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewReceiveOnlyExchangeBuffer(settings)

	totalMessageCount := benchmark.N * exchangeTcpBenchmarkBurstMessageCount
	readResult := make(chan error, 1)
	consumeDone := make(chan struct{})
	dispatchCount := 0
	if 0 < maximumBatchMessageCount {
		batchQueueSize := max(
			1,
			settings.ExchangeBufferSize/maximumBatchMessageCount,
		)
		dispatch := make(chan *exchangeTcpBenchmarkDispatchBatch, batchQueueSize)
		available := make(chan *exchangeTcpBenchmarkDispatchBatch, batchQueueSize)
		for range batchQueueSize {
			available <- &exchangeTcpBenchmarkDispatchBatch{}
		}
		go func() {
			for batch := range dispatch {
				for messageIndex, message := range batch.messages[:batch.messageCount] {
					connectlib.MessagePoolReturn(message)
					batch.messages[messageIndex] = nil
				}
				batch.messageCount = 0
				available <- batch
			}
			close(consumeDone)
		}()
		go func() {
			defer close(dispatch)
			remainingMessageCount := totalMessageCount
			for 0 < remainingMessageCount {
				batch := <-available
				if err := readExchangeTcpBenchmarkReadyBatch(
					receiveBuffer,
					reader,
					batch,
					maximumBatchMessageCount,
				); err != nil {
					available <- batch
					readResult <- err
					return
				}
				if remainingMessageCount < batch.messageCount {
					readyMessageCount := batch.messageCount
					for messageIndex, message := range batch.messages[:batch.messageCount] {
						connectlib.MessagePoolReturn(message)
						batch.messages[messageIndex] = nil
					}
					batch.messageCount = 0
					available <- batch
					readResult <- fmt.Errorf(
						"ready batch exceeded remaining messages (%d<%d)",
						remainingMessageCount,
						readyMessageCount,
					)
					return
				}
				remainingMessageCount -= batch.messageCount
				dispatchCount += 1
				dispatch <- batch
			}
			readResult <- nil
		}()
	} else {
		dispatch := make(chan []byte, settings.ExchangeBufferSize)
		go func() {
			for message := range dispatch {
				connectlib.MessagePoolReturn(message)
			}
			close(consumeDone)
		}()
		go func() {
			defer close(dispatch)
			for range totalMessageCount {
				message, err := receiveBuffer.ReadMessage(reader)
				if err != nil {
					readResult <- err
					return
				}
				if len(message) != exchangeTcpBenchmarkMessageByteCount {
					connectlib.MessagePoolReturn(message)
					readResult <- fmt.Errorf(
						"exchange message byte count = %d, want %d",
						len(message),
						exchangeTcpBenchmarkMessageByteCount,
					)
					return
				}
				dispatchCount += 1
				dispatch <- message
			}
			readResult <- nil
		}()
	}

	benchmark.ReportAllocs()
	benchmark.SetBytes(
		int64(exchangeTcpBenchmarkBurstMessageCount * exchangeTcpBenchmarkMessageByteCount),
	)
	benchmark.ResetTimer()

	var writeErr error
	for burstIndex := range benchmark.N {
		var messages [exchangeTcpBenchmarkBurstMessageCount][]byte
		for messageIndex := range messages {
			messages[messageIndex] = newExchangeTcpBenchmarkMessage(
				burstIndex*exchangeTcpBenchmarkBurstMessageCount + messageIndex,
			)
		}
		if writeErr = sendBuffer.WriteMessages(writer, messages[:]); writeErr != nil {
			break
		}
	}
	if writeErr != nil {
		writer.Close()
	}
	readErr := <-readResult
	<-consumeDone
	benchmark.StopTimer()

	if writeErr != nil {
		benchmark.Fatal(writeErr)
	}
	if readErr != nil {
		benchmark.Fatal(readErr)
	}
	benchmark.ReportMetric(
		float64(totalMessageCount)/float64(dispatchCount),
		"messages/dispatch",
	)
}

// Measures singleton decoded-message handoff after the real framed TCP reader.
func BenchmarkExchangeTcpDispatchSingleton(benchmark *testing.B) {
	runExchangeTcpDispatchBenchmark(benchmark, 0)
}

// Measures bounded decoded-batch handoff after the real framed TCP reader.
func BenchmarkExchangeTcpDispatchBatch(benchmark *testing.B) {
	runExchangeTcpDispatchBenchmark(
		benchmark,
		exchangeTcpBenchmarkBurstMessageCount,
	)
}

// Measures decoded-message read-ahead with an eight-message descriptor.
func BenchmarkExchangeTcpDispatchBatch8(benchmark *testing.B) {
	runExchangeTcpDispatchBenchmark(benchmark, 8)
}

// Measures the previously evaluated 64-message read-ahead descriptor.
func BenchmarkExchangeTcpDispatchBatch64(benchmark *testing.B) {
	runExchangeTcpDispatchBenchmark(
		benchmark,
		exchangeTcpBenchmarkBurstMessageCount,
	)
}

// Measures sparse one-message latency with no sender backlog. Every operation
// waits for the separate dispatch consumer, so the ready-batch variant cannot
// borrow throughput from later messages or hide latency behind queue depth.
func runExchangeTcpSparseDispatchBenchmark(
	benchmark *testing.B,
	batchDispatch bool,
) {
	settings := DefaultExchangeSettings()
	writer, reader := newExchangeTcpBenchmarkPair(benchmark)
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewReceiveOnlyExchangeBuffer(settings)
	dispatchCompleted := make(chan struct{}, 1)
	readErrors := make(chan error, 1)
	readDone := make(chan struct{})
	consumeDone := make(chan struct{})

	if batchDispatch {
		dispatch := make(chan *exchangeTcpBenchmarkDispatchBatch, 1)
		available := make(chan *exchangeTcpBenchmarkDispatchBatch, 1)
		available <- &exchangeTcpBenchmarkDispatchBatch{}
		go func() {
			for batch := range dispatch {
				for messageIndex, message := range batch.messages[:batch.messageCount] {
					connectlib.MessagePoolReturn(message)
					batch.messages[messageIndex] = nil
				}
				batch.messageCount = 0
				available <- batch
				dispatchCompleted <- struct{}{}
			}
			close(consumeDone)
		}()
		go func() {
			defer close(readDone)
			defer close(dispatch)
			for range benchmark.N {
				batch := <-available
				if err := readExchangeTcpBenchmarkReadyBatch(
					receiveBuffer,
					reader,
					batch,
					exchangeTcpBenchmarkBurstMessageCount,
				); err != nil {
					available <- batch
					readErrors <- err
					return
				}
				if batch.messageCount != 1 {
					readyMessageCount := batch.messageCount
					for messageIndex, message := range batch.messages[:batch.messageCount] {
						connectlib.MessagePoolReturn(message)
						batch.messages[messageIndex] = nil
					}
					batch.messageCount = 0
					available <- batch
					readErrors <- fmt.Errorf(
						"sparse ready batch message count = %d, want 1",
						readyMessageCount,
					)
					return
				}
				dispatch <- batch
			}
		}()
	} else {
		dispatch := make(chan []byte, 1)
		go func() {
			for message := range dispatch {
				connectlib.MessagePoolReturn(message)
				dispatchCompleted <- struct{}{}
			}
			close(consumeDone)
		}()
		go func() {
			defer close(readDone)
			defer close(dispatch)
			for range benchmark.N {
				message, err := receiveBuffer.ReadMessage(reader)
				if err != nil {
					readErrors <- err
					return
				}
				if err = validateExchangeTcpBenchmarkMessage(message); err != nil {
					connectlib.MessagePoolReturn(message)
					readErrors <- err
					return
				}
				dispatch <- message
			}
		}()
	}

	benchmark.ReportAllocs()
	benchmark.SetBytes(exchangeTcpBenchmarkMessageByteCount)
	benchmark.ResetTimer()

	var resultErr error
	for messageIndex := range benchmark.N {
		message := newExchangeTcpBenchmarkMessage(messageIndex)
		if resultErr = sendBuffer.WriteMessage(writer, message); resultErr != nil {
			break
		}
		select {
		case <-dispatchCompleted:
		case resultErr = <-readErrors:
		}
		if resultErr != nil {
			break
		}
	}
	if resultErr != nil {
		writer.Close()
	}
	<-readDone
	<-consumeDone
	benchmark.StopTimer()

	if resultErr != nil {
		benchmark.Fatal(resultErr)
	}
}

// Measures sparse singleton dispatch latency over real framed TCP.
func BenchmarkExchangeTcpDispatchSparseSingletonLatency(benchmark *testing.B) {
	runExchangeTcpSparseDispatchBenchmark(benchmark, false)
}

// Measures sparse ready-only batch dispatch latency over real framed TCP.
func BenchmarkExchangeTcpDispatchSparseReadyBatchLatency(benchmark *testing.B) {
	runExchangeTcpSparseDispatchBenchmark(benchmark, true)
}

// Records production ExchangeBuffer flush boundaries while its read side
// remains blocked until connection teardown. SetWriteDeadline starts one
// WriteMessages flush; the generic net.Conn path then writes alternating frame
// headers and bodies within that boundary.
type exchangeOutboundBatchRecorderConn struct {
	stateLock sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}
	message   chan struct{}
	batches   [][]int
	header    [4]byte
	headerLen int
	body      []byte
}

// Creates a recorder with enough completion notifications for one burst.
func newExchangeOutboundBatchRecorderConn() *exchangeOutboundBatchRecorderConn {
	return &exchangeOutboundBatchRecorderConn{
		closed:  make(chan struct{}),
		message: make(chan struct{}, exchangeTcpBenchmarkBurstMessageCount),
	}
}

// Blocks the production receive worker until connection ownership closes.
func (self *exchangeOutboundBatchRecorderConn) Read([]byte) (int, error) {
	<-self.closed
	return 0, io.EOF
}

// Records framed bytes from the current production flush. net.Buffers may
// combine adjacent iovecs when the destination is not a TCPConn, so parsing
// the byte stream keeps the boundary recorder independent of Write chunking.
func (self *exchangeOutboundBatchRecorderConn) Write(buffer []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.batches) == 0 {
		return 0, errors.New("exchange write began outside a flush")
	}
	writtenByteCount := len(buffer)
	for 0 < len(buffer) {
		if self.headerLen < len(self.header) {
			copiedByteCount := copy(self.header[self.headerLen:], buffer)
			self.headerLen += copiedByteCount
			buffer = buffer[copiedByteCount:]
			if self.headerLen < len(self.header) {
				continue
			}
			messageByteCount := int(binary.BigEndian.Uint16(self.header[0:2]))
			self.body = make([]byte, 0, messageByteCount)
		}
		remainingByteCount := cap(self.body) - len(self.body)
		copiedByteCount := min(remainingByteCount, len(buffer))
		self.body = append(self.body, buffer[:copiedByteCount]...)
		buffer = buffer[copiedByteCount:]
		if len(self.body) < cap(self.body) {
			continue
		}
		if err := validateExchangeTcpBenchmarkMessage(self.body); err != nil {
			return 0, err
		}
		batchIndex := len(self.batches) - 1
		self.batches[batchIndex] = append(self.batches[batchIndex], int(self.body[0]))
		self.headerLen = 0
		self.body = nil
		self.message <- struct{}{}
	}
	return writtenByteCount, nil
}

// Releases the blocked read side once.
func (self *exchangeOutboundBatchRecorderConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

// Returns a synthetic loopback address.
func (self *exchangeOutboundBatchRecorderConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

// Returns a synthetic loopback address.
func (self *exchangeOutboundBatchRecorderConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

// Applies both directions independently in the production connection.
func (self *exchangeOutboundBatchRecorderConn) SetDeadline(time.Time) error {
	return nil
}

// Leaves the blocked read controlled by Close.
func (self *exchangeOutboundBatchRecorderConn) SetReadDeadline(time.Time) error {
	return nil
}

// Starts one exact production WriteMessage or WriteMessages flush boundary.
func (self *exchangeOutboundBatchRecorderConn) SetWriteDeadline(time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.headerLen != 0 || self.body != nil {
		return errors.New("exchange flush ended after a frame header")
	}
	self.batches = append(self.batches, nil)
	return nil
}

// Copies the recorded message ids grouped by production flush.
func (self *exchangeOutboundBatchRecorderConn) snapshot() [][]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	batches := make([][]int, len(self.batches))
	for batchIndex, batch := range self.batches {
		batches[batchIndex] = append([]int(nil), batch...)
	}
	return batches
}

// Constructs the production outbound socket writer without its database-backed
// dial loop. The transport op still runs its real receive worker; the forward
// op runs its real discard reader.
func newExchangeOutboundBatchConnection(
	op ExchangeOp,
) (*ExchangeConnection, *exchangeOutboundBatchRecorderConn) {
	settings := DefaultExchangeSettings()
	settings.ExchangeBufferSize = exchangeTcpBenchmarkBurstMessageCount
	settings.ExchangePingTimeout = time.Hour
	settings.WriteTimeout = time.Hour
	recorder := newExchangeOutboundBatchRecorderConn()
	ctx, cancel := context.WithCancel(context.Background())
	connection := &ExchangeConnection{
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		conn:          recorder,
		sendBuffer:    NewDefaultExchangeBuffer(settings),
		receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
		send:          make(chan []byte, exchangeTcpBenchmarkBurstMessageCount),
		receive:       make(chan []byte, exchangeTcpBenchmarkBurstMessageCount),
		settings:      settings,
		header:        ExchangeHeader{Op: op},
	}
	return connection, recorder
}

// Waits on a correctness barrier with a timeout used only as a failure guard.
func waitExchangeOutboundBarrier(testingT testing.TB, barrier <-chan struct{}, description string) {
	testingT.Helper()
	select {
	case <-barrier:
	case <-time.After(5 * time.Second):
		testingT.Fatalf("timed out waiting for %s", description)
	}
}

// Runs one exact burst through either a directly prefilled accepted-side queue
// or the outbound resident queue bridge. The outbound ordering forces the
// production writer to own its first batch after exactly one bridge transfer;
// the remaining transfers complete while that first socket flush is held.
func runExchangeOutboundBatchFormation(
	testingT testing.TB,
	op ExchangeOp,
	bridge bool,
) [][]int {
	testingT.Helper()
	connection, recorder := newExchangeOutboundBatchConnection(op)
	firstBatchOwned := make(chan struct{})
	releaseFirstFlush := make(chan struct{})
	var firstBatchOnce sync.Once
	connection.afterSendDequeueForTest = func() {
		firstBatchOnce.Do(func() {
			close(firstBatchOwned)
			<-releaseFirstFlush
		})
	}

	source := connection.send
	if bridge {
		source = make(chan []byte, exchangeTcpBenchmarkBurstMessageCount)
	}
	witnesses := make([][]byte, 0, exchangeTcpBenchmarkBurstMessageCount)
	for messageIndex := range exchangeTcpBenchmarkBurstMessageCount {
		message := newExchangeTcpBenchmarkMessage(messageIndex)
		witnesses = append(witnesses, connectlib.MessagePoolShareReadOnly(message))
		source <- message
	}

	go connection.Run()
	var bridgeDone chan struct{}
	var firstBridged chan struct{}
	var releaseBridge chan struct{}
	if bridge {
		bridgeDone = make(chan struct{})
		firstBridged = make(chan struct{})
		releaseBridge = make(chan struct{})
		go func() {
			defer close(bridgeDone)
			writeTimer := time.NewTimer(0)
			defer writeTimer.Stop()
			for messageIndex := range exchangeTcpBenchmarkBurstMessageCount {
				message := <-source
				result := connection.sendMessage(
					connection.Done(),
					message,
					writeTimer,
					time.Hour,
				)
				if result != pooledMessageSendDelivered {
					return
				}
				if messageIndex == 0 {
					close(firstBridged)
					<-releaseBridge
				}
			}
		}()
		waitExchangeOutboundBarrier(testingT, firstBridged, "first resident bridge transfer")
	}

	waitExchangeOutboundBarrier(testingT, firstBatchOwned, "first exchange writer batch")
	if bridge {
		close(releaseBridge)
		waitExchangeOutboundBarrier(testingT, bridgeDone, "remaining resident bridge transfers")
	}
	close(releaseFirstFlush)
	for messageIndex := range exchangeTcpBenchmarkBurstMessageCount {
		select {
		case <-recorder.message:
		case <-time.After(5 * time.Second):
			testingT.Fatalf(
				"timed out after %d recorded exchange messages: %v",
				messageIndex,
				recorder.snapshot(),
			)
		}
	}
	connection.Close()

	if len(source) != 0 || len(connection.send) != 0 {
		testingT.Fatalf(
			"exchange queues remain source=%d connection=%d",
			len(source),
			len(connection.send),
		)
	}
	for witnessIndex, witness := range witnesses {
		if !connectlib.MessagePoolReturn(witness) {
			testingT.Fatalf("message %d retained another pooled owner", witnessIndex)
		}
	}
	return recorder.snapshot()
}

// Verifies exact flush formation, FIFO, and pooled ownership across both
// accepted socket writers and both outbound resident queue bridges.
func TestExchangeOutboundBatchFormation(t *testing.T) {
	testCases := []struct {
		name        string
		op          ExchangeOp
		bridge      bool
		batchCounts []int
	}{
		{name: "accepted transport", op: ExchangeOpTransport, batchCounts: []int{64}},
		{name: "accepted forward", op: ExchangeOpForward, batchCounts: []int{64}},
		{name: "resident transport", op: ExchangeOpTransport, bridge: true, batchCounts: []int{1, 63}},
		{name: "resident forward", op: ExchangeOpForward, bridge: true, batchCounts: []int{1, 63}},
	}
	for _, testCase := range testCases {
		batches := runExchangeOutboundBatchFormation(t, testCase.op, testCase.bridge)
		if len(batches) != len(testCase.batchCounts) {
			t.Errorf("%s flush count = %d, want %d: %v", testCase.name, len(batches), len(testCase.batchCounts), batches)
			continue
		}
		nextMessageIndex := 0
		for batchIndex, batch := range batches {
			if len(batch) != testCase.batchCounts[batchIndex] {
				t.Errorf("%s flush %d message count = %d, want %d", testCase.name, batchIndex, len(batch), testCase.batchCounts[batchIndex])
			}
			for _, messageIndex := range batch {
				if messageIndex != nextMessageIndex {
					t.Errorf("%s message = %d, want FIFO index %d", testCase.name, messageIndex, nextMessageIndex)
				}
				nextMessageIndex += 1
			}
		}
		if nextMessageIndex != exchangeTcpBenchmarkBurstMessageCount {
			t.Errorf("%s recorded message count = %d, want %d", testCase.name, nextMessageIndex, exchangeTcpBenchmarkBurstMessageCount)
		}
	}
}

// Reports accepted transport batch formation through the production writer.
func BenchmarkExchangeAcceptedTransportBatchFormation(benchmark *testing.B) {
	var flushCount int
	for range benchmark.N {
		flushCount += len(runExchangeOutboundBatchFormation(benchmark, ExchangeOpTransport, false))
	}
	benchmark.ReportMetric(float64(exchangeTcpBenchmarkBurstMessageCount*benchmark.N)/float64(flushCount), "messages/flush")
}

// Reports accepted forward batch formation through the production writer.
func BenchmarkExchangeAcceptedForwardBatchFormation(benchmark *testing.B) {
	var flushCount int
	for range benchmark.N {
		flushCount += len(runExchangeOutboundBatchFormation(benchmark, ExchangeOpForward, false))
	}
	benchmark.ReportMetric(float64(exchangeTcpBenchmarkBurstMessageCount*benchmark.N)/float64(flushCount), "messages/flush")
}

// Reports current resident transport bridge formation through the production
// queue admission and socket writer.
func BenchmarkExchangeResidentTransportBatchFormation(benchmark *testing.B) {
	var flushCount int
	for range benchmark.N {
		flushCount += len(runExchangeOutboundBatchFormation(benchmark, ExchangeOpTransport, true))
	}
	benchmark.ReportMetric(float64(exchangeTcpBenchmarkBurstMessageCount*benchmark.N)/float64(flushCount), "messages/flush")
}

// Reports current resident forward bridge formation through the production
// queue admission and socket writer.
func BenchmarkExchangeResidentForwardBatchFormation(benchmark *testing.B) {
	var flushCount int
	for range benchmark.N {
		flushCount += len(runExchangeOutboundBatchFormation(benchmark, ExchangeOpForward, true))
	}
	benchmark.ReportMetric(float64(exchangeTcpBenchmarkBurstMessageCount*benchmark.N)/float64(flushCount), "messages/flush")
}
