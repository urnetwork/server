package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The composed forward fixture over a real loopback exchange listener and the
// real accept path, with the forward's loop running. The destination's forward
// consumer counts and takes each message, the same on both paths, so the
// difference is the hop.
func newLocalForwardBenchFixture(b *testing.B, local bool, delivered *atomic.Int64) *localForwardComposedFixture {
	f := newLocalForwardComposedFixture(b, local, nil)
	f.recordResident.forwardReceiveForTest = func(_ context.Context, message []byte) bool {
		delivered.Add(1)
		clientconnect.MessagePoolReturn(message)
		return true
	}
	listenConfig := exchangeListenConfig(exchangeKeepAliveConfig(f.settings))
	listener, err := listenConfig.Listen(f.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	// the fixture's cancel closes the listener
	stopListener := context.AfterFunc(f.ctx, func() {
		listener.Close()
	})
	f.socketWorkers.Add(1)
	go func() {
		defer f.socketWorkers.Done()
		defer stopListener()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.socketWorkers.Add(1)
			go func() {
				defer f.socketWorkers.Done()
				f.exchange.handleExchangeConnection(conn)
			}()
		}
	}()
	f.settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		f.dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	f.run()
	return f
}

// One forward's 1,000-byte frames from its send queue to its destination
// resident's forward consumer, in process or over a real loopback exchange
// socket and the real accept path, with production exchange settings
// otherwise. Reports process CPU per frame beside wall time and allocations.
func BenchmarkForwardFrameLocalVsTcpLoop(b *testing.B) {
	warmLocalTransportPool()
	for _, local := range []bool{true, false} {
		name := "tcp_loop"
		if local {
			name = "local"
		}
		b.Run(name, func(b *testing.B) {
			var delivered atomic.Int64
			f := newLocalForwardBenchFixture(b, local, &delivered)
			defer f.close()
			sent := int64(0)
			send := func(count int) {
				for range count {
					frame := clientconnect.MessagePoolGet(1000)
					frame[0] = 0xff
					select {
					case f.forward.send <- frame:
					case <-f.ctx.Done():
						clientconnect.MessagePoolReturn(frame)
						b.Fatal("the forward ended")
					}
					sent += 1
				}
				endTime := time.Now().Add(5 * time.Minute)
				for delivered.Load() < sent {
					if endTime.Before(time.Now()) {
						b.Fatalf("delivered %d of %d", delivered.Load(), sent)
					}
					time.Sleep(100 * time.Microsecond)
				}
			}
			send(1000)
			if local != (f.dials.Load() == 0) {
				b.Fatalf("local=%t dialed %d exchange sockets", local, f.dials.Load())
			}

			b.ReportAllocs()
			cpuBefore := localTransportBenchProcessCpu(b)
			b.ResetTimer()
			send(b.N)
			b.StopTimer()
			cpu := localTransportBenchProcessCpu(b) - cpuBefore
			b.ReportMetric(float64(cpu.Nanoseconds())/float64(b.N)/1000, "cpu-us/frame")
		})
	}
}
