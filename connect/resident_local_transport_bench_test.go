package connect

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Process CPU, user and system, so the loopback's kernel work counts.
func localTransportBenchProcessCpu(b *testing.B) time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// The composed fixture over a real loopback exchange listener and the real
// accept path, with the transport's loop running.
func newLocalTransportBenchFixture(b *testing.B, local bool) *localTransportComposedFixture {
	f := newLocalTransportComposedFixture(b, local, nil)
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

// One transport's frames from the device client to its resident's internal
// client, delivered in process or over a real loopback exchange socket to the
// real accept path, with production exchange settings otherwise. Reports
// process CPU per frame beside wall time and allocations.
func BenchmarkTransportFrameLocalVsTcpLoop(b *testing.B) {
	warmLocalTransportPool()
	for _, local := range []bool{true, false} {
		name := "tcp_loop"
		if local {
			name = "local"
		}
		b.Run(name, func(b *testing.B) {
			f := newLocalTransportBenchFixture(b, local)
			defer f.close()

			received := f.received(f.recordResident)
			payload := strings.Repeat("x", 1000)
			sent := 0
			send := func(count int) {
				for range count {
					frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: fmt.Sprintf("%09d%s", sent, payload)})
					if !f.device.SendWithTimeout(frame, clientconnect.ControlId, func(error) {}, time.Minute) {
						clientconnect.MessagePoolReturn(frame.MessageBytes)
						b.Fatal("device refused a frame")
					}
					sent += 1
				}
				waitCtx, waitCancel := context.WithTimeout(f.ctx, 5*time.Minute)
				defer waitCancel()
				received.wait(b, waitCtx, sent, "resident")
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

// The hop alone: raw 1,000-byte frames from the transport's send queue to the
// resident's receive route, in process or over a real loopback exchange socket
// and the real accept path. The resident's client drops each undecodable frame
// at once on both paths, so the difference is the hop: syscalls, deadlines,
// framing copies and wakes against two channel hand-offs.
func BenchmarkTransportFrameHopLocalVsTcpLoop(b *testing.B) {
	warmLocalTransportPool()
	for _, local := range []bool{true, false} {
		name := "tcp_loop"
		if local {
			name = "local"
		}
		b.Run(name, func(b *testing.B) {
			f := newLocalTransportBenchFixture(b, local)
			defer f.close()
			delivered := func() float64 {
				if local {
					return snapshotLocalTransportIO("received", "data").frames
				}
				return snapshotExchangeIOOp("transport", "received", "data").frames
			}
			send := func(count int) {
				start := delivered()
				for range count {
					frame := clientconnect.MessagePoolGet(1000)
					frame[0] = 0xff
					select {
					case f.transport.send <- frame:
					case <-f.ctx.Done():
						b.Fatal("the transport ended")
					}
				}
				endTime := time.Now().Add(5 * time.Minute)
				for delivered()-start < float64(count) {
					if endTime.Before(time.Now()) {
						b.Fatalf("delivered %v of %d", delivered()-start, count)
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
