// Frames per socket call on a real loopback exchange link (LBALTOPT2.md R21).
// The sender is the production outbound writer (ExchangeConnection.Run), fed
// through the production queue admission (sendMessage) by a bridge goroutine,
// as a resident forward feeds it; the receiver is the production read path
// (ExchangeBuffer.ReadMessage) on the accepted socket. Each benchmark sends
// bursts of one shape and reports the frames a write call and a read call
// carry, from exchange_io_frames_total and exchange_io_calls_total.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	connectlib "github.com/urnetwork/connect"
)

// The forward op's sent and received frames and calls so far.
type exchangeIOCallsSnapshot struct {
	sentFrames    float64
	sentCalls     float64
	receivedFrame float64
	receivedCalls float64
}

func snapshotExchangeIOCalls() exchangeIOCallsSnapshot {
	frames := func(direction exchangeIODirection) float64 {
		total := 0.0
		for kind := exchangeIOFrameKind(0); kind < exchangeIOFrameKindCount; kind++ {
			total += testutil.ToFloat64(exchangeIOCollectorsByLabel[exchangeIOOpForward][direction][kind].frames)
		}
		return total
	}
	return exchangeIOCallsSnapshot{
		sentFrames:    frames(exchangeIODirectionSent),
		sentCalls:     testutil.ToFloat64(exchangeIOCallsByLabel[exchangeIOOpForward][exchangeIODirectionSent]),
		receivedFrame: frames(exchangeIODirectionReceived),
		receivedCalls: testutil.ToFloat64(exchangeIOCallsByLabel[exchangeIOOpForward][exchangeIODirectionReceived]),
	}
}

// Sends b.N bursts of burstCount frames of frameByteCount bytes and reads
// each burst back before the next.
func runExchangeIOCallsBenchmark(b *testing.B, burstCount int, frameByteCount int) {
	writerConn, readerConn := newExchangeTcpBenchmarkPair(b)
	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = time.Hour
	settings.WriteTimeout = time.Hour
	settings.ExchangeReadTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	sendBuffer := NewDefaultExchangeBuffer(settings)
	sendBuffer.setIOOp(ExchangeOpForward)
	connection := &ExchangeConnection{
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		conn:          writerConn,
		sendBuffer:    sendBuffer,
		receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
		send:          make(chan []byte, settings.ExchangeBufferSize),
		receive:       make(chan []byte),
		settings:      settings,
		header:        ExchangeHeader{Op: ExchangeOpForward},
	}
	go connection.Run()
	defer connection.Close()

	// the bridge: a resident forward's queue to the connection's admission
	upstream := make(chan []byte, settings.ForwardBufferSize)
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		writeTimer := time.NewTimer(0)
		defer writeTimer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-upstream:
				if result := connection.sendMessage(ctx.Done(), message, writeTimer, time.Hour); result != pooledMessageSendDelivered {
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-bridgeDone
		returnReadyPooledMessages(upstream)
	}()

	readBuffer := NewReceiveOnlyExchangeBuffer(settings)
	readBuffer.setIOOp(ExchangeOpForward)
	before := snapshotExchangeIOCalls()
	b.ResetTimer()
	for range b.N {
		for i := range burstCount {
			message := connectlib.MessagePoolGet(frameByteCount)
			message[0] = byte(i)
			upstream <- message
		}
		for range burstCount {
			message, err := readBuffer.ReadMessage(readerConn)
			if err != nil {
				b.Fatal(err)
			}
			if len(message) != frameByteCount {
				b.Fatalf("read a %d-byte frame, want %d", len(message), frameByteCount)
			}
			connectlib.MessagePoolReturn(message)
		}
	}
	b.StopTimer()
	frames := float64(b.N * burstCount)
	// the writer records a write's frames after its call returns, which can
	// be after the reader has them
	after := snapshotExchangeIOCalls()
	for deadline := time.Now().Add(5 * time.Second); after.sentFrames-before.sentFrames < frames && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
		after = snapshotExchangeIOCalls()
	}
	if sent := after.sentFrames - before.sentFrames; sent != frames {
		b.Fatalf("recorded %.0f sent frames, want %.0f", sent, frames)
	}
	b.ReportMetric(frames/(after.sentCalls-before.sentCalls), "frames/write")
	b.ReportMetric(frames/(after.receivedCalls-before.receivedCalls), "frames/read")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/frames, "ns/frame")
}

// A lone ACK-sized frame at a time: sparse traffic, the common case on a link.
func BenchmarkExchangeIOCallsLoneAck(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 1, 100)
}

// A lone packet-sized frame at a time.
func BenchmarkExchangeIOCallsLonePacket(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 1, 1380)
}

// A lone frame larger than the writer's singleton scratch.
func BenchmarkExchangeIOCallsLoneLarge(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 1, 4096)
}

// A burst of 16 ACK-sized frames.
func BenchmarkExchangeIOCallsBurst16Ack(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 16, 100)
}

// A burst of 16 packet-sized frames, a TCP window's worth of a flow.
func BenchmarkExchangeIOCallsBurst16Packet(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 16, 1380)
}

// A burst of 64 packet-sized frames.
func BenchmarkExchangeIOCallsBurst64Packet(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 64, 1380)
}

// A burst of 8 frames larger than the singleton scratch.
func BenchmarkExchangeIOCallsBurst8Large(b *testing.B) {
	runExchangeIOCallsBenchmark(b, 8, 4096)
}
