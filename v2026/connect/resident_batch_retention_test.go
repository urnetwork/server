package connect

import (
	"context"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"
)

// Discard writes without retaining the caller's slice; the reader stays alive
// until the test closes the connection. This exercises the actual idle socket
// writer rather than treating a returned pool counter as proof of collection.
type exchangeDiscardRetentionConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *exchangeDiscardRetentionConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}
func (c *exchangeDiscardRetentionConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *exchangeDiscardRetentionConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *exchangeDiscardRetentionConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *exchangeDiscardRetentionConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *exchangeDiscardRetentionConn) SetDeadline(time.Time) error      { return nil }
func (c *exchangeDiscardRetentionConn) SetReadDeadline(time.Time) error  { return nil }
func (c *exchangeDiscardRetentionConn) SetWriteDeadline(time.Time) error { return nil }

// A completed burst must become collectible while its writer remains alive,
// including when the next batch is smaller. These deliberately unpooled large
// buffers isolate stale writer references from legitimate free-pool retention.
func TestExchangeLiveWriterReleasesPreviousBatchBacking(t *testing.T) {
	for _, op := range []ExchangeOp{ExchangeOpTransport, ExchangeOpForward} {
		t.Run(exchangeOpMetricLabel(op), func(t *testing.T) {
			settings := DefaultExchangeSettings()
			settings.ExchangeWriteBatchCount = 8
			settings.ExchangePingTimeout = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			conn := &exchangeDiscardRetentionConn{closed: make(chan struct{})}
			connection := &ExchangeConnection{
				ctx: ctx, cancel: cancel, done: make(chan struct{}), conn: conn,
				sendBuffer: NewDefaultExchangeBuffer(settings), receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
				send: make(chan []byte, 8), receive: make(chan []byte, 8),
				settings: settings, header: ExchangeHeader{Op: op},
			}
			secondBatch := make(chan struct{})
			releaseSecond := make(chan struct{})
			var releaseOnce sync.Once
			batchCount := 0
			connection.afterSendDequeueForTest = func() {
				batchCount++
				if batchCount == 2 {
					close(secondBatch)
					<-releaseSecond
				}
			}
			defer func() {
				releaseOnce.Do(func() { close(releaseSecond) })
				connection.Close()
			}()
			witnesses := enqueueExchangeRetentionBurst(connection.send)
			go connection.Run()
			// This send follows the prefilled burst. The production writer's
			// second-dequeue barrier proves the first WriteMessages has returned.
			connection.send <- []byte{1}
			waitExchangeOutboundBarrier(t, secondBatch, "smaller second batch")
			runtime.GC()
			runtime.GC()
			retained := 0
			for _, witness := range witnesses {
				if witness.Value() != nil {
					retained++
				}
			}
			runtime.KeepAlive(connection)
			if retained != 0 {
				t.Fatalf("live idle writer retains %d completed unpooled payloads", retained)
			}
		})
	}
}

// Keep the enqueue-time strong references out of the collecting test frame.
func enqueueExchangeRetentionBurst(queue chan<- []byte) []weak.Pointer[byte] {
	witnesses := make([]weak.Pointer[byte], 8)
	for i := range witnesses {
		payload := make([]byte, 16*1024)
		payload[0] = byte(i + 1)
		witnesses[i] = weak.Make(&payload[0])
		queue <- payload
	}
	return witnesses
}
