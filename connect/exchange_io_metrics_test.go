package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

type exchangeWriteBudgetConn struct {
	remaining int
	err       error
}

func (c *exchangeWriteBudgetConn) Read([]byte) (int, error) {
	return 0, net.ErrClosed
}

func (c *exchangeWriteBudgetConn) Write(bytes []byte) (int, error) {
	if c.remaining == 0 {
		return 0, c.err
	}
	if len(bytes) <= c.remaining {
		c.remaining -= len(bytes)
		return len(bytes), nil
	}
	written := c.remaining
	c.remaining = 0
	return written, c.err
}

func (c *exchangeWriteBudgetConn) Close() error                     { return nil }
func (c *exchangeWriteBudgetConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *exchangeWriteBudgetConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *exchangeWriteBudgetConn) SetDeadline(time.Time) error      { return nil }
func (c *exchangeWriteBudgetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *exchangeWriteBudgetConn) SetWriteDeadline(time.Time) error { return nil }

type exchangeIOMetricSnapshot struct {
	frames float64
	bytes  float64
}

func snapshotExchangeIO(direction string, kind string) exchangeIOMetricSnapshot {
	return exchangeIOMetricSnapshot{
		frames: testutil.ToFloat64(exchangeIOFramesCounter.WithLabelValues(direction, kind)),
		bytes:  testutil.ToFloat64(exchangeIOBytesCounter.WithLabelValues(direction, kind)),
	}
}

func requireExchangeIODelta(
	t *testing.T,
	direction string,
	kind string,
	before exchangeIOMetricSnapshot,
	wantFrames float64,
	wantBytes float64,
) {
	t.Helper()
	after := snapshotExchangeIO(direction, kind)
	if delta := after.frames - before.frames; delta != wantFrames {
		t.Errorf("%s %s frame delta = %v, want %v", direction, kind, delta, wantFrames)
	}
	if delta := after.bytes - before.bytes; delta != wantBytes {
		t.Errorf("%s %s byte delta = %v, want %v", direction, kind, delta, wantBytes)
	}
}

func TestExchangeBufferRecordsCompletedFramesOnce(t *testing.T) {
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	senderConn, receiverConn := net.Pipe()
	t.Cleanup(func() {
		senderConn.Close()
		receiverConn.Close()
	})

	t.Run("handshake", func(t *testing.T) {
		sentBefore := snapshotExchangeIO("sent", "handshake")
		receivedBefore := snapshotExchangeIO("received", "handshake")
		header := &ExchangeHeader{
			Version:    1,
			ClientId:   server.NewId(),
			ResidentId: server.NewId(),
			Op:         ExchangeOpTransport,
		}
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- sender.WriteHeader(context.Background(), senderConn, header)
		}()
		if _, err := receiver.ReadHeader(context.Background(), receiverConn); err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}

		sentAfter := snapshotExchangeIO("sent", "handshake")
		sentBytes := sentAfter.bytes - sentBefore.bytes
		if sentBytes <= exchangeIOFrameHeaderByteCount {
			t.Fatalf("handshake wire bytes = %v, want a framed payload", sentBytes)
		}
		requireExchangeIODelta(t, "sent", "handshake", sentBefore, 1, sentBytes)
		requireExchangeIODelta(t, "received", "handshake", receivedBefore, 1, sentBytes)
	})

	t.Run("batched data", func(t *testing.T) {
		messageLengths := []int{3, 17, 513}
		batch := make([][]byte, 0, len(messageLengths))
		wantBytes := 0
		for messageIndex, messageLength := range messageLengths {
			message := connectlib.MessagePoolGet(messageLength)
			for byteIndex := range message {
				message[byteIndex] = byte(messageIndex + 1)
			}
			batch = append(batch, message)
			wantBytes += exchangeIOFrameHeaderByteCount + messageLength
		}
		sentBefore := snapshotExchangeIO("sent", "data")
		receivedBefore := snapshotExchangeIO("received", "data")
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- sender.WriteMessages(senderConn, batch)
		}()
		for messageIndex, messageLength := range messageLengths {
			message, err := receiver.ReadMessage(receiverConn)
			if err != nil {
				t.Fatal(err)
			}
			if len(message) != messageLength || message[0] != byte(messageIndex+1) {
				t.Errorf("message %d = len %d prefix %d, want len %d prefix %d", messageIndex, len(message), message[0], messageLength, messageIndex+1)
			}
			connectlib.MessagePoolReturn(message)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		requireExchangeIODelta(t, "sent", "data", sentBefore, float64(len(messageLengths)), float64(wantBytes))
		requireExchangeIODelta(t, "received", "data", receivedBefore, float64(len(messageLengths)), float64(wantBytes))
	})

	t.Run("ping", func(t *testing.T) {
		sentBefore := snapshotExchangeIO("sent", "ping")
		receivedBefore := snapshotExchangeIO("received", "ping")
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- sender.WriteMessage(senderConn, connectlib.MessagePoolGet(0))
		}()
		message, err := receiver.ReadMessage(receiverConn)
		if err != nil {
			t.Fatal(err)
		}
		connectlib.MessagePoolReturn(message)
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		requireExchangeIODelta(t, "sent", "ping", sentBefore, 1, exchangeIOFrameHeaderByteCount)
		requireExchangeIODelta(t, "received", "ping", receivedBefore, 1, exchangeIOFrameHeaderByteCount)
	})
}

func TestExchangeBufferDoesNotRecordFailedFrame(t *testing.T) {
	settings := DefaultExchangeSettings()
	buffer := NewDefaultExchangeBuffer(settings)
	localConn, peerConn := net.Pipe()
	peerConn.Close()
	defer localConn.Close()

	before := snapshotExchangeIO("sent", "data")
	message := connectlib.MessagePoolGet(32)
	if err := buffer.WriteMessage(localConn, message); err == nil {
		t.Fatal("write to a closed exchange peer unexpectedly succeeded")
	}
	requireExchangeIODelta(t, "sent", "data", before, 0, 0)
}

func TestExchangeBufferRecordsOnlyCompletedBatchPrefix(t *testing.T) {
	settings := DefaultExchangeSettings()
	buffer := NewDefaultExchangeBuffer(settings)
	messageLengths := []int{23, 41, 67}
	batch := make([][]byte, 0, len(messageLengths))
	for _, messageLength := range messageLengths {
		batch = append(batch, connectlib.MessagePoolGet(messageLength))
	}
	firstFrameByteCount := exchangeIOFrameHeaderByteCount + messageLengths[0]
	conn := &exchangeWriteBudgetConn{
		remaining: firstFrameByteCount,
		err:       errors.New("injected batch write failure"),
	}

	before := snapshotExchangeIO("sent", "data")
	if err := buffer.WriteMessages(conn, batch); !errors.Is(err, conn.err) {
		t.Fatalf("batch write error = %v, want %v", err, conn.err)
	}
	requireExchangeIODelta(t, "sent", "data", before, 1, float64(firstFrameByteCount))
}

func TestExchangeConnectionTracksActiveOutboundEndpoint(t *testing.T) {
	for _, op := range []ExchangeOp{ExchangeOpTransport, ExchangeOpForward} {
		opLabel := exchangeOpMetricLabel(op)
		t.Run(opLabel, func(t *testing.T) {
			before := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", opLabel))
			clientConn, serverConn := net.Pipe()
			settings := DefaultExchangeSettings()
			settings.ExchangePingTimeout = time.Hour
			serverDone := make(chan error, 1)
			go func() {
				serverDone <- serveExchangeHeaderEcho(serverConn, settings)
			}()
			settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
				return clientConn, nil
			}
			connection, err := NewExchangeConnection(
				context.Background(),
				ExchangeHeader{
					Version:    1,
					ClientId:   server.NewId(),
					ResidentId: server.NewId(),
					Op:         op,
				},
				"edge-a",
				18443,
				nil,
				settings,
			)
			if err != nil {
				t.Fatal(err)
			}
			if active := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", opLabel)); active != before+1 {
				t.Fatalf("active outbound %s = %v, want %v", opLabel, active, before+1)
			}
			connection.Close()
			if active := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", opLabel)); active != before {
				t.Fatalf("active outbound %s after close = %v, want %v", opLabel, active, before)
			}
			select {
			case serverErr := <-serverDone:
				if !isExpectedExchangeFixtureCloseError(serverErr) {
					t.Fatal(serverErr)
				}
			case <-time.After(time.Second):
				t.Fatal("exchange fixture did not observe connection close")
			}
		})
	}
}
