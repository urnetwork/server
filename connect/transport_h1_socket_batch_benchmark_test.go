// Server H1 socket benchmarks compare singleton WebSocket writes with two
// ready-only batching shapes over real loopback TCP and TLS/TCP connections.
// The test listener inserts a pass-through wrapper below TLS; the coalesced
// variant brackets only complete WebSocket frames already waiting in the
// transport queue, so every variant preserves the production wire format.
package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	serverH1SocketBatchBenchmarkPayloadByteCount = 1380
	serverH1SocketBatchBenchmarkQueueSize        = 4096
	serverH1SocketBatchBenchmarkMessageCount     = 4
	serverH1SocketBatchBenchmarkMaxMessageCount  = 8
	serverH1SocketBatchBenchmarkByteCount        = 16 * 1024
)

// Selects whether every next message is already queued or is released only
// after the preceding message reaches the receiving WebSocket application.
type serverH1SocketBatchBenchmarkWorkload int

const (
	serverH1SocketBatchBenchmarkSaturated serverH1SocketBatchBenchmarkWorkload = iota
	serverH1SocketBatchBenchmarkSparse
)

// Selects how the server writer consumes one ready burst.
type serverH1SocketBatchBenchmarkMode int

const (
	serverH1SocketBatchBenchmarkSingleton serverH1SocketBatchBenchmarkMode = iota
	serverH1SocketBatchBenchmarkReadySeparate
	serverH1SocketBatchBenchmarkReadyCoalesced
)

// Configures one explicit carrier, writer shape, and arrival pattern.
type serverH1SocketBatchBenchmarkSettings struct {
	mode            serverH1SocketBatchBenchmarkMode
	workload        serverH1SocketBatchBenchmarkWorkload
	enableTls       bool
	maxMessageCount int
}

// Counts writes into the transport wrapper separately from writes that reach
// the real TCP connection. With TLS enabled it also parses complete TLS records
// from the encrypted byte stream. Batching state is confined to the server's
// one writer; reads and deadlines retain net.Conn's concurrent contract.
type serverH1SocketBatchBenchmarkConn struct {
	net.Conn

	writeBuffer []byte
	batching    bool

	connectionWriteCount atomic.Uint64
	tcpWriteCount        atomic.Uint64

	countTlsRecords        bool
	tlsRecordStateLock     sync.Mutex
	tlsRecordHeader        [5]byte
	tlsRecordHeaderLen     int
	tlsRecordPayloadRemain int
	tlsRecordCount         uint64
}

// Enables one explicit ready-only write batch.
func (self *serverH1SocketBatchBenchmarkConn) beginWriteBatch() {
	if self.batching {
		panic("server H1 benchmark write batch already active")
	}
	if self.writeBuffer == nil {
		self.writeBuffer = make(
			[]byte,
			0,
			serverH1SocketBatchBenchmarkByteCount,
		)
	} else {
		self.writeBuffer = self.writeBuffer[:0]
	}
	self.batching = true
}

// Discards bytes retained after a failed logical WebSocket write.
func (self *serverH1SocketBatchBenchmarkConn) abortWriteBatch() {
	self.batching = false
	self.writeBuffer = self.writeBuffer[:0]
}

// Performs one counted write on the real loopback TCP connection.
func (self *serverH1SocketBatchBenchmarkConn) writeSocket(buffer []byte) (int, error) {
	self.tcpWriteCount.Add(1)
	return self.Conn.Write(buffer)
}

// Parses record boundaries across arbitrary TLS-to-connection write splits.
func (self *serverH1SocketBatchBenchmarkConn) noteTlsBytes(buffer []byte) {
	if !self.countTlsRecords {
		return
	}
	self.tlsRecordStateLock.Lock()
	defer self.tlsRecordStateLock.Unlock()
	for len(buffer) != 0 {
		if 0 < self.tlsRecordPayloadRemain {
			consumedByteCount := min(self.tlsRecordPayloadRemain, len(buffer))
			self.tlsRecordPayloadRemain -= consumedByteCount
			buffer = buffer[consumedByteCount:]
			continue
		}
		if self.tlsRecordHeaderLen < len(self.tlsRecordHeader) {
			copiedByteCount := copy(
				self.tlsRecordHeader[self.tlsRecordHeaderLen:],
				buffer,
			)
			self.tlsRecordHeaderLen += copiedByteCount
			buffer = buffer[copiedByteCount:]
			if self.tlsRecordHeaderLen < len(self.tlsRecordHeader) {
				continue
			}
			self.tlsRecordPayloadRemain =
				int(self.tlsRecordHeader[3])*256 + int(self.tlsRecordHeader[4])
			self.tlsRecordHeaderLen = 0
			self.tlsRecordCount += 1
		}
	}
}

// Snapshots the number of complete TLS record headers observed so far.
func (self *serverH1SocketBatchBenchmarkConn) loadTlsRecordCount() uint64 {
	self.tlsRecordStateLock.Lock()
	defer self.tlsRecordStateLock.Unlock()
	return self.tlsRecordCount
}

// Reports whether the encrypted stream ends on an exact TLS record boundary.
func (self *serverH1SocketBatchBenchmarkConn) tlsRecordStateComplete() bool {
	self.tlsRecordStateLock.Lock()
	defer self.tlsRecordStateLock.Unlock()
	return self.tlsRecordHeaderLen == 0 && self.tlsRecordPayloadRemain == 0
}

// Flushes retained complete WebSocket frame bytes without ending the batch.
func (self *serverH1SocketBatchBenchmarkConn) flushWriteBuffer() error {
	if len(self.writeBuffer) == 0 {
		return nil
	}
	writeByteCount := len(self.writeBuffer)
	writtenByteCount, err := self.writeSocket(self.writeBuffer)
	self.writeBuffer = self.writeBuffer[:0]
	if err == nil && writtenByteCount != writeByteCount {
		return io.ErrShortWrite
	}
	return err
}

// Ends one batch and emits all retained frames in the fewest bounded writes.
func (self *serverH1SocketBatchBenchmarkConn) flushWriteBatch() error {
	if !self.batching {
		return nil
	}
	self.batching = false
	return self.flushWriteBuffer()
}

// Passes through outside an explicit batch and otherwise retains complete
// Gorilla WebSocket writes until the batch flush boundary.
func (self *serverH1SocketBatchBenchmarkConn) Write(buffer []byte) (int, error) {
	self.connectionWriteCount.Add(1)
	self.noteTlsBytes(buffer)
	if !self.batching {
		return self.writeSocket(buffer)
	}
	if serverH1SocketBatchBenchmarkByteCount < len(self.writeBuffer)+len(buffer) {
		if err := self.flushWriteBuffer(); err != nil {
			return 0, err
		}
	}
	if serverH1SocketBatchBenchmarkByteCount < len(buffer) {
		return self.writeSocket(buffer)
	}
	self.writeBuffer = append(self.writeBuffer, buffer...)
	return len(buffer), nil
}

// Wraps accepted TCP sockets before net/http and Gorilla observe them.
type serverH1SocketBatchBenchmarkListener struct {
	net.Listener
	countTlsRecords bool
}

// Returns a pass-through counting connection for one HTTP upgrade.
func (self *serverH1SocketBatchBenchmarkListener) Accept() (net.Conn, error) {
	connection, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &serverH1SocketBatchBenchmarkConn{
		Conn:            connection,
		countTlsRecords: self.countTlsRecords,
	}, nil
}

// Carries writer completion and the number of ready batches it observed.
type serverH1SocketBatchBenchmarkWriterResult struct {
	err                   error
	readyBatchCount       int
	writeDeadlineSetCount int
}

// Runs one server-outbound WebSocket transfer through a persistent real TCP or
// TLS/TCP connection. Saturated runs preload the bounded queue; sparse runs use
// an application-delivery barrier to prove ready-only batching adds no wait.
func benchmarkServerH1SocketBatch(
	b *testing.B,
	settings serverH1SocketBatchBenchmarkSettings,
) {
	b.Helper()
	b.SetBytes(serverH1SocketBatchBenchmarkPayloadByteCount)
	maxMessageCount := settings.maxMessageCount
	if maxMessageCount == 0 {
		maxMessageCount = serverH1SocketBatchBenchmarkMessageCount
	}
	if maxMessageCount < 1 ||
		serverH1SocketBatchBenchmarkMaxMessageCount < maxMessageCount {
		b.Fatalf("invalid maximum message count %d", maxMessageCount)
	}

	payload := make([]byte, serverH1SocketBatchBenchmarkPayloadByteCount)
	for i := range payload {
		payload[i] = byte(i)
	}

	benchmarkCtx, benchmarkCancel := context.WithCancel(context.Background())
	queueSize := min(max(b.N, 1), serverH1SocketBatchBenchmarkQueueSize)
	if settings.workload == serverH1SocketBatchBenchmarkSparse {
		queueSize = 0
	}
	send := make(chan []byte, queueSize)
	preloadedMessageCount := 0
	if settings.workload == serverH1SocketBatchBenchmarkSaturated {
		preloadedMessageCount = min(b.N, queueSize)
	}
	for range preloadedMessageCount {
		send <- payload
	}
	start := make(chan struct{})
	handlerRelease := make(chan struct{})
	var handlerReleaseOnce sync.Once
	releaseHandler := func() {
		handlerReleaseOnce.Do(func() {
			close(handlerRelease)
		})
	}
	connectionReady := make(chan *serverH1SocketBatchBenchmarkConn, 1)
	writerResult := make(chan serverH1SocketBatchBenchmarkWriterResult, 1)

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4 * 1024,
		WriteBufferSize: 4 * 1024,
		CheckOrigin: func(request *http.Request) bool {
			return true
		},
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		webSocketConnection, upgradeErr := upgrader.Upgrade(response, request, nil)
		if upgradeErr != nil {
			writerResult <- serverH1SocketBatchBenchmarkWriterResult{
				err: fmt.Errorf("upgrade: %w", upgradeErr),
			}
			return
		}
		defer webSocketConnection.Close()

		var connection *serverH1SocketBatchBenchmarkConn
		underlyingConnection := webSocketConnection.UnderlyingConn()
		switch typedConnection := underlyingConnection.(type) {
		case *serverH1SocketBatchBenchmarkConn:
			connection = typedConnection
		case *tls.Conn:
			connection, _ = typedConnection.NetConn().(*serverH1SocketBatchBenchmarkConn)
		}
		if connection == nil {
			writerResult <- serverH1SocketBatchBenchmarkWriterResult{
				err: fmt.Errorf(
					"unexpected server WebSocket connection %T",
					underlyingConnection,
				),
			}
			return
		}
		connectionReady <- connection
		select {
		case <-benchmarkCtx.Done():
			writerResult <- serverH1SocketBatchBenchmarkWriterResult{
				err: benchmarkCtx.Err(),
			}
			return
		case <-start:
		}

		writtenMessageCount := 0
		readyBatchCount := 0
		writeDeadlineSetCount := 0
		var messageStorage [serverH1SocketBatchBenchmarkMaxMessageCount][]byte
		for writtenMessageCount < b.N {
			var firstMessage []byte
			select {
			case <-benchmarkCtx.Done():
				writerResult <- serverH1SocketBatchBenchmarkWriterResult{
					err:                   benchmarkCtx.Err(),
					readyBatchCount:       readyBatchCount,
					writeDeadlineSetCount: writeDeadlineSetCount,
				}
				return
			case firstMessage = <-send:
			}

			messages := messageStorage[:1:maxMessageCount]
			messages[0] = firstMessage
			if settings.mode != serverH1SocketBatchBenchmarkSingleton {
			drainReady:
				for len(messages) < cap(messages) &&
					writtenMessageCount+len(messages) < b.N {
					select {
					case message := <-send:
						messages = append(messages, message)
					default:
						break drainReady
					}
				}
			}

			readyBatchCount += 1
			webSocketConnection.SetWriteDeadline(time.Now().Add(30 * time.Second))
			writeDeadlineSetCount += 1
			if settings.mode == serverH1SocketBatchBenchmarkReadyCoalesced {
				connection.beginWriteBatch()
			}
			writeSucceeded := true
			for _, message := range messages {
				if writeErr := webSocketConnection.WriteMessage(
					websocket.BinaryMessage,
					message,
				); writeErr != nil {
					if settings.mode == serverH1SocketBatchBenchmarkReadyCoalesced {
						connection.abortWriteBatch()
					}
					writerResult <- serverH1SocketBatchBenchmarkWriterResult{
						err:                   writeErr,
						readyBatchCount:       readyBatchCount,
						writeDeadlineSetCount: writeDeadlineSetCount,
					}
					writeSucceeded = false
					break
				}
			}
			if !writeSucceeded {
				return
			}
			if settings.mode == serverH1SocketBatchBenchmarkReadyCoalesced {
				if flushErr := connection.flushWriteBatch(); flushErr != nil {
					writerResult <- serverH1SocketBatchBenchmarkWriterResult{
						err:                   flushErr,
						readyBatchCount:       readyBatchCount,
						writeDeadlineSetCount: writeDeadlineSetCount,
					}
					return
				}
			}
			writtenMessageCount += len(messages)
		}
		writerResult <- serverH1SocketBatchBenchmarkWriterResult{
			readyBatchCount:       readyBatchCount,
			writeDeadlineSetCount: writeDeadlineSetCount,
		}
		select {
		case <-benchmarkCtx.Done():
		case <-handlerRelease:
		}
	})

	testServer := httptest.NewUnstartedServer(handler)
	testServer.Listener = &serverH1SocketBatchBenchmarkListener{
		Listener:        testServer.Listener,
		countTlsRecords: settings.enableTls,
	}
	if settings.enableTls {
		testServer.StartTLS()
	} else {
		testServer.Start()
	}
	b.Cleanup(testServer.Close)

	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		ReadBufferSize:   4 * 1024,
		WriteBufferSize:  4 * 1024,
	}
	if settings.enableTls {
		transport, ok := testServer.Client().Transport.(*http.Transport)
		if !ok || transport.TLSClientConfig == nil {
			b.Fatalf("unexpected TLS test transport %T", testServer.Client().Transport)
		}
		dialer.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	clientConnection, response, err := dialer.Dial(
		"ws"+strings.TrimPrefix(testServer.URL, "http"),
		nil,
	)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		benchmarkCancel()
		b.Fatal(err)
	}
	b.Cleanup(func() {
		releaseHandler()
		benchmarkCancel()
		clientConnection.Close()
	})

	var connection *serverH1SocketBatchBenchmarkConn
	select {
	case connection = <-connectionReady:
	case result := <-writerResult:
		b.Fatalf("writer setup: %v", result.err)
	}
	if settings.enableTls && !connection.tlsRecordStateComplete() {
		b.Fatal("TLS setup did not finish on a complete record boundary")
	}
	if settings.mode == serverH1SocketBatchBenchmarkReadyCoalesced {
		// Measure steady-state frame coalescing. The one bounded buffer is a
		// per-connection setup cost, not a per-batch allocation.
		connection.beginWriteBatch()
		connection.abortWriteBatch()
	}
	connectionWriteCountBefore := connection.connectionWriteCount.Load()
	tcpWriteCountBefore := connection.tcpWriteCount.Load()
	tlsRecordCountBefore := connection.loadTlsRecordCount()

	readerReady := make(chan struct{})
	delivered := make(chan struct{}, 1)
	readerResult := make(chan error, 1)
	go func() {
		close(readerReady)
		receivedPayload := make([]byte, len(payload))
		for range b.N {
			messageType, reader, readErr := clientConnection.NextReader()
			if readErr != nil {
				readerResult <- readErr
				return
			}
			if messageType != websocket.BinaryMessage {
				readerResult <- fmt.Errorf("unexpected message type %d", messageType)
				return
			}
			if _, readErr = io.ReadFull(reader, receivedPayload); readErr != nil {
				readerResult <- readErr
				return
			}
			if receivedPayload[0] != payload[0] ||
				receivedPayload[len(receivedPayload)-1] != payload[len(payload)-1] {
				readerResult <- errors.New("received payload changed")
				return
			}
			var extra [1]byte
			if extraByteCount, extraErr := reader.Read(extra[:]); extraByteCount != 0 || !errors.Is(extraErr, io.EOF) {
				readerResult <- fmt.Errorf(
					"message boundary extra bytes=%d err=%v",
					extraByteCount,
					extraErr,
				)
				return
			}
			if settings.workload == serverH1SocketBatchBenchmarkSparse {
				delivered <- struct{}{}
			}
		}
		readerResult <- nil
	}()
	<-readerReady

	b.ReportAllocs()
	b.ResetTimer()
	close(start)
	readerCompleted := false
	if settings.workload == serverH1SocketBatchBenchmarkSparse {
		for range b.N {
			select {
			case send <- payload:
			case result := <-writerResult:
				b.StopTimer()
				b.Fatalf("writer stopped while releasing sparse frame: %v", result.err)
			}
			select {
			case <-delivered:
			case readErr := <-readerResult:
				if readErr != nil {
					b.StopTimer()
					b.Fatalf("sparse frame was not delivered: %v", readErr)
				}
				// Successful completion is published after the final delivery
				// edge. Consume that already-ready edge instead of treating the
				// select's arbitrary choice as a benchmark failure.
				<-delivered
				readerCompleted = true
			}
		}
	} else {
		for range b.N - preloadedMessageCount {
			select {
			case send <- payload:
			case result := <-writerResult:
				b.StopTimer()
				b.Fatalf("writer stopped while producing: %v", result.err)
			}
		}
	}
	result := <-writerResult
	var readErr error
	if !readerCompleted {
		readErr = <-readerResult
	}
	b.StopTimer()

	if result.err != nil {
		b.Fatal(result.err)
	}
	if readErr != nil {
		b.Fatal(readErr)
	}
	connectionWriteCount := connection.connectionWriteCount.Load() - connectionWriteCountBefore
	tcpWriteCount := connection.tcpWriteCount.Load() - tcpWriteCountBefore
	tlsRecordCount := connection.loadTlsRecordCount() - tlsRecordCountBefore
	if !settings.enableTls && connectionWriteCount != uint64(b.N) {
		b.Fatalf("WebSocket connection writes=%d, want %d", connectionWriteCount, b.N)
	}
	if settings.enableTls && (tlsRecordCount == 0 || !connection.tlsRecordStateComplete()) {
		b.Fatalf(
			"invalid TLS record accounting: records=%d boundary-complete=%t",
			tlsRecordCount,
			connection.tlsRecordStateComplete(),
		)
	}
	if tcpWriteCount == 0 || result.readyBatchCount == 0 {
		b.Fatalf(
			"empty write accounting: TCP=%d ready batches=%d",
			tcpWriteCount,
			result.readyBatchCount,
		)
	}
	if settings.workload == serverH1SocketBatchBenchmarkSparse &&
		result.readyBatchCount != b.N {
		b.Fatalf(
			"sparse ready batches=%d, want exactly one per frame (%d)",
			result.readyBatchCount,
			b.N,
		)
	}
	if settings.workload == serverH1SocketBatchBenchmarkSparse {
		b.ReportMetric(
			float64(b.Elapsed().Nanoseconds())/float64(b.N),
			"delivery-ns/frame",
		)
	}
	b.ReportMetric(
		float64(connectionWriteCount)/float64(b.N),
		"underlay-writes/frame",
	)
	b.ReportMetric(
		float64(tcpWriteCount)/float64(b.N),
		"tcp-writes/frame",
	)
	b.ReportMetric(
		float64(b.N)/float64(tcpWriteCount),
		"frames/tcp-write",
	)
	b.ReportMetric(
		float64(b.N)/float64(result.readyBatchCount),
		"frames/ready-batch",
	)
	b.ReportMetric(
		float64(result.writeDeadlineSetCount)/float64(b.N),
		"write-deadlines/frame",
	)
	if settings.enableTls {
		b.ReportMetric(
			float64(tlsRecordCount)/float64(b.N),
			"tls-records/frame",
		)
		b.ReportMetric(
			float64(b.N)/float64(tlsRecordCount),
			"frames/tls-record",
		)
	}
	releaseHandler()
}

// Measures the current server H1 one-message-per-WebSocket-write behavior.
func BenchmarkServerH1WebSocketSingletonLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkSingleton,
		workload:  serverH1SocketBatchBenchmarkSaturated,
		enableTls: false,
	})
}

// Measures ready draining while retaining one socket write per WebSocket frame.
func BenchmarkServerH1WebSocketReadyDrainSeparateLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadySeparate,
		workload:  serverH1SocketBatchBenchmarkSaturated,
		enableTls: false,
	})
}

// Measures ready draining with four complete frames per bounded socket write.
func BenchmarkServerH1WebSocketReadyDrainCoalescedLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:  serverH1SocketBatchBenchmarkSaturated,
		enableTls: false,
	})
}

// Measures production-like TLS with the current singleton H1 writer.
func BenchmarkServerH1WebSocketTlsSingletonLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkSingleton,
		workload:  serverH1SocketBatchBenchmarkSaturated,
		enableTls: true,
	})
}

// Measures TLS after ready draining without coalescing encrypted records.
func BenchmarkServerH1WebSocketTlsReadyDrainSeparateLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadySeparate,
		workload:  serverH1SocketBatchBenchmarkSaturated,
		enableTls: true,
	})
}

// Measures TLS after four ready WebSocket frames share one bounded TCP write.
func BenchmarkServerH1WebSocketTlsReadyDrainCoalescedLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:  serverH1SocketBatchBenchmarkSaturated,
		enableTls: true,
	})
}

// Measures four-message cleartext coalescing with every other axis fixed.
func BenchmarkServerH1WebSocketReadyDrainCoalescedBatch4Loopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:            serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:        serverH1SocketBatchBenchmarkSaturated,
		enableTls:       false,
		maxMessageCount: 4,
	})
}

// Measures eight-message cleartext coalescing with every other axis fixed.
func BenchmarkServerH1WebSocketReadyDrainCoalescedBatch8Loopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:            serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:        serverH1SocketBatchBenchmarkSaturated,
		enableTls:       false,
		maxMessageCount: 8,
	})
}

// Measures four-message TLS coalescing with every other axis fixed.
func BenchmarkServerH1WebSocketTlsReadyDrainCoalescedBatch4Loopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:            serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:        serverH1SocketBatchBenchmarkSaturated,
		enableTls:       true,
		maxMessageCount: 4,
	})
}

// Measures eight-message TLS coalescing with every other axis fixed.
func BenchmarkServerH1WebSocketTlsReadyDrainCoalescedBatch8Loopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:            serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:        serverH1SocketBatchBenchmarkSaturated,
		enableTls:       true,
		maxMessageCount: 8,
	})
}

// Measures isolated cleartext frames with the current singleton H1 writer.
func BenchmarkServerH1WebSocketSparseSingletonLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkSingleton,
		workload:  serverH1SocketBatchBenchmarkSparse,
		enableTls: false,
	})
}

// Proves ready draining does not wait for an absent second cleartext frame.
func BenchmarkServerH1WebSocketSparseReadyDrainSeparateLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadySeparate,
		workload:  serverH1SocketBatchBenchmarkSparse,
		enableTls: false,
	})
}

// Proves coalescing immediately flushes a lone cleartext frame.
func BenchmarkServerH1WebSocketSparseReadyDrainCoalescedLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:  serverH1SocketBatchBenchmarkSparse,
		enableTls: false,
	})
}

// Measures isolated TLS frames with the current singleton H1 writer.
func BenchmarkServerH1WebSocketTlsSparseSingletonLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkSingleton,
		workload:  serverH1SocketBatchBenchmarkSparse,
		enableTls: true,
	})
}

// Proves ready draining does not wait for an absent second TLS frame.
func BenchmarkServerH1WebSocketTlsSparseReadyDrainSeparateLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadySeparate,
		workload:  serverH1SocketBatchBenchmarkSparse,
		enableTls: true,
	})
}

// Proves coalescing immediately flushes a lone TLS record at its ready boundary.
func BenchmarkServerH1WebSocketTlsSparseReadyDrainCoalescedLoopback(b *testing.B) {
	benchmarkServerH1SocketBatch(b, serverH1SocketBatchBenchmarkSettings{
		mode:      serverH1SocketBatchBenchmarkReadyCoalesced,
		workload:  serverH1SocketBatchBenchmarkSparse,
		enableTls: true,
	})
}
