// Controls for the instance-owned, nil-default observation seam. These are
// not substitutes for the six real Client/Resident writer-boundary roots.
package connect

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The new constructor publication precedes a real worker's first hook read.
func TestExchangeConnectionPublishesPrivateHooksBeforeWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	settings := DefaultExchangeSettings()
	peerDone := make(chan error, 1)
	peerStarted, peerJoined := false, false
	var connection *ExchangeConnection
	var witness []byte
	defer func() {
		cancel()
		if connection != nil {
			connection.Close()
		}
		if peerStarted && !peerJoined {
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer joinCancel()
			select {
			case <-peerDone:
			case <-joinCtx.Done():
				t.Error("publication peer did not join after cancellation")
			}
		}
		if witness != nil && !clientconnect.MessagePoolReturn(witness) {
			t.Error("joined publication control retained another pooled owner")
		}
	}()
	published := atomic.Bool{}
	dequeued := make(chan struct{}, 1)
	outcomes := make(chan pooledMessageSendResult, 1)
	header := ExchangeHeader{Op: ExchangeOpForward}
	settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		local, remote := net.Pipe()
		peerStarted = true
		go func() {
			defer remote.Close()
			buffer := NewReceiveOnlyExchangeBuffer(settings)
			got, err := buffer.ReadHeader(ctx, remote)
			if err == nil && got.Op != header.Op {
				err = fmt.Errorf("real header changed operation")
			}
			if err == nil {
				err = buffer.WriteHeader(ctx, remote, got)
			}
			if err == nil {
				var message []byte
				message, err = buffer.ReadMessage(remote)
				if err == nil && string(message) != "publication-control" {
					err = fmt.Errorf("real framed body changed")
				}
				clientconnect.MessagePoolReturn(message)
			}
			peerDone <- err
		}()
		return local, nil
	}
	settings.beforeExchangeConnectionRunForTest = func(connection *ExchangeConnection) {
		connection.afterSendDequeueForTest = func() {
			if !published.Load() {
				panic("connection worker ran before private publication completed")
			}
			select {
			case dequeued <- struct{}{}:
			default:
			}
		}
		connection.afterSendMessageForTest = func(result pooledMessageSendResult, _ int) {
			select {
			case outcomes <- result:
			default:
			}
		}
		published.Store(true)
	}
	var err error
	connection, err = NewExchangeConnection(ctx, header, "publication.example", 1, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	message := clientconnect.MessagePoolCopy([]byte("publication-control"))
	witness = clientconnect.MessagePoolShareReadOnly(message)
	timer := time.NewTimer(0)
	defer timer.Stop()
	if result := connection.sendMessage(ctx.Done(), message, timer, settings.WriteTimeout); result != pooledMessageSendDelivered {
		t.Fatalf("real constructor send disposition=%v", result)
	}
	select {
	case <-dequeued:
	case <-ctx.Done():
		t.Fatal("real writer did not observe prepublished hook")
	}
	select {
	case result := <-outcomes:
		if result != pooledMessageSendDelivered {
			t.Fatalf("scalar consumed-owner result=%v", result)
		}
	case <-ctx.Done():
		t.Fatal("actual send did not publish its scalar result")
	}
	select {
	case err := <-peerDone:
		peerJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("real header/body peer did not join")
	}
}

// The default path retains no observer closure or per-message allocation.
func TestExchangeConnectionNilOutcomeObserverAllocatesZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultExchangeSettings()
	connection := &ExchangeConnection{ctx: ctx, send: make(chan []byte, settings.ExchangeBufferSize)}
	if settings.beforeExchangeConnectionRunForTest != nil || connection.afterSendMessageForTest != nil {
		t.Fatal("private observations are not nil by default")
	}
	t.Logf("observation field bytes: settings=%d connection=%d",
		unsafe.Sizeof(settings.beforeExchangeConnectionRunForTest), unsafe.Sizeof(connection.afterSendMessageForTest))
	message := []byte("nil-observer-control")
	timer := time.NewTimer(0)
	defer timer.Stop()
	allocations := testing.AllocsPerRun(1000, func() {
		if connection.sendMessage(nil, message, timer, settings.WriteTimeout) != pooledMessageSendDelivered {
			panic("ready observer control refused")
		}
		<-connection.send
	})
	if allocations != 0 {
		t.Fatalf("nil observer admitted path allocated %g times per message", allocations)
	}
}

// Measures the unavoidable nil check in a ready default path; no production
// throughput claim is inferred from this isolated ownership microbenchmark.
func BenchmarkExchangeConnectionSendMessageNilOutcomeObserver(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultExchangeSettings()
	connection := &ExchangeConnection{ctx: ctx, send: make(chan []byte, settings.ExchangeBufferSize)}
	message := []byte("nil-observer-benchmark")
	timer := time.NewTimer(0)
	defer timer.Stop()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if connection.sendMessage(nil, message, timer, settings.WriteTimeout) != pooledMessageSendDelivered {
			b.Fatal("ready observer benchmark refused")
		}
		<-connection.send
	}
}
