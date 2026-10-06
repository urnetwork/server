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

// Embedding the TCP connection preserves net.Buffers' native writev path.
// Write calls distinguish that path from its ordinary net.Conn fallback.
type exchangeWritevObservedTCPConn struct {
	*net.TCPConn
	writes       atomic.Int64
	deadlines    atomic.Int64
	closeAtBatch bool
}

func (c *exchangeWritevObservedTCPConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.TCPConn.Write(p)
}

func (c *exchangeWritevObservedTCPConn) SetWriteDeadline(deadline time.Time) error {
	err := c.TCPConn.SetWriteDeadline(deadline)
	if c.deadlines.Add(1) == 3 && c.closeAtBatch && err == nil {
		// The header and singleton have completed. Close the real socket only
		// after the batch deadline succeeds, so WriteTo itself sees the error.
		_ = c.TCPConn.Close()
	}
	return err
}

// A failed native writev can consume only an iovec prefix. Closing and joining
// the owning ExchangeConnection must leave no borrowed payload or header
// pointers in the reusable vector, even while that closed owner stays alive.
func TestExchangeConnectionWritevReleasesTerminalBatchReferences(t *testing.T) {
	for _, op := range []ExchangeOp{ExchangeOpTransport, ExchangeOpForward} {
		for _, outcome := range []string{"success", "partial_timeout", "immediate_close"} {
			t.Run(exchangeOpMetricLabel(op)+"/"+outcome, func(t *testing.T) {
				testExchangeConnectionWritevTerminalBatch(t, op, outcome)
			})
		}
	}
}

func testExchangeConnectionWritevTerminalBatch(t *testing.T, op ExchangeOp, outcome string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = time.Hour
	settings.WriteTimeout = time.Second
	const batchCount = 32
	const payloadSize = 8 * 1024
	if settings.ExchangeBufferSize != 4096 || settings.ExchangeWriteBatchCount != 256 ||
		settings.ExchangeWriteBatchByteCount != batchCount*payloadSize || settings.FramerSettings.MaxMessageLen != 16*1024 {
		t.Fatal("fixture no longer exercises the resident's production queue and batch bounds")
	}

	peerFinished := make(chan error, 1)
	peerReadBatch := make(chan struct{})
	peerRelease := make(chan struct{})
	peerSettings := *settings
	// Keep read-ahead finite and small while the peer deliberately stops
	// partway through the second payload. Framing is unchanged.
	peerSettings.ExchangeReadBufferByteCount = 16
	go func() {
		peerFinished <- func() error {
			peer, err := listener.AcceptTCP()
			if err != nil {
				return err
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
			select {
			case <-peerRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}()
	}()

	var socket *exchangeWritevObservedTCPConn
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
		socket = &exchangeWritevObservedTCPConn{TCPConn: tcp, closeAtBatch: outcome == "immediate_close"}
		return socket, nil
	}
	connection, err := NewExchangeConnection(ctx, ExchangeHeader{
		Version: 1, ClientId: server.NewId(), ResidentId: server.NewId(), Op: op,
	}, "fixture", 1, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	firstDequeued := make(chan struct{})
	releaseFirst := make(chan struct{})
	var dequeuedBatches int
	var writesBeforeBatch int64
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
		}
	}
	before := snapshotExchangeIO("sent", "data")
	first := clientconnect.MessagePoolCopy([]byte("first"))
	witnesses := [][]byte{retainResidentPoolWitness(first)}
	if result := connection.sendMessage(ctx.Done(), first, nil, time.Second); result != pooledMessageSendDelivered {
		t.Fatalf("first admission = %v", result)
	}
	select {
	case <-firstDequeued:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Two accepted frames share each root, while a third reference stays in the
	// test. This detects missing or duplicate returns without relying on GC.
	for i := range batchCount / 2 {
		message := clientconnect.MessagePoolCopy(bytes.Repeat([]byte{byte(i + 1)}, payloadSize))
		if cap(message) != payloadSize+clientconnect.MessagePoolMetaByteCount {
			t.Fatal("expected an exact pooled protocol-frame root")
		}
		shared := clientconnect.MessagePoolShareReadOnly(message)
		witnesses = append(witnesses, retainResidentPoolWitness(message))
		for _, offered := range [][]byte{message, shared} {
			if result := connection.sendMessage(ctx.Done(), offered, nil, time.Second); result != pooledMessageSendDelivered {
				t.Fatalf("batch admission = %v", result)
			}
		}
	}
	close(releaseFirst)
	if outcome == "success" {
		select {
		case <-peerReadBatch:
		case err := <-peerFinished:
			t.Fatalf("peer ended before complete batch: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	} else {
		// The real failed write cancels Run. Done is cancellation; Close below
		// additionally joins socket workers and drains both ownership queues.
		select {
		case <-connection.Done():
		case <-ctx.Done():
			t.Fatal("native batch did not terminate")
		}
	}
	connection.Close()
	close(peerRelease)
	select {
	case err := <-peerFinished:
		if err != nil {
			t.Fatal(err)
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
	if dequeuedBatches != 2 || socket.deadlines.Load() != 3 || socket.writes.Load() != writesBeforeBatch {
		t.Fatalf("expected header, singleton, then one native writev: batches=%d deadlines=%d fallback writes=%d",
			dequeuedBatches, socket.deadlines.Load(), socket.writes.Load()-writesBeforeBatch)
	}
	if len(connection.send) != 0 || len(connection.receive) != 0 {
		t.Fatal("joined connection retained queued messages")
	}
	requireResidentPoolOwnersReturned(t, witnesses, "joined native writev")
	after := snapshotExchangeIO("sent", "data")
	frames, wireBytes := after.frames-before.frames, after.bytes-before.bytes
	switch outcome {
	case "success":
		requireExchangeIODelta(t, "sent", "data", before, batchCount+1, 9+batchCount*(payloadSize+4))
	case "immediate_close":
		requireExchangeIODelta(t, "sent", "data", before, 1, 9)
	case "partial_timeout":
		if frames < 2 || batchCount+1 <= frames || wireBytes != 9+(frames-1)*(payloadSize+4) {
			t.Errorf("partial metrics = %v frames/%v bytes, want a completed frame prefix only", frames, wireBytes)
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
		outcome, frames, wireBytes, len(vector), cap(vector), retained, batchCount/2)
	if retained != 0 {
		t.Errorf("closed ExchangeConnection retains %d borrowed writev references after returning payload ownership", retained)
	}
	// Deliberately retain the completed owner through the backing-array check;
	// reclaiming the whole ExchangeConnection cannot make this assertion pass.
	runtime.KeepAlive(connection)
}
