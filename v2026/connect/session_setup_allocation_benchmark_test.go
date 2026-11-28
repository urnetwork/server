// The allocation of one client session's setup in connect (LBALTOPT2.md
// §3.5, R23): its resident transport, one resident forward, and the outbound
// exchange connection of each, dialed over loopback through the real header
// handshake. Sessions last a median 4.4 s in production, so this is paid at
// the session rate. The session's six queues are messageQueues, which allocate
// as they fill; they were channels made at their bound, ExchangeBufferSize and
// ForwardBufferSize messages each, 590 KB of the setup's 698 KB.
package connect

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Accepts exchange connections and echoes each one's header frame, as an
// accepting exchange does, then discards what follows.
func startExchangeHeaderEchoForTest(t testing.TB) (host string, port int) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var frameHeader [exchangeIOFrameHeaderByteCount]byte
				if _, err := io.ReadFull(conn, frameHeader[:]); err != nil {
					return
				}
				body := make([]byte, int(frameHeader[0])<<8|int(frameHeader[1]))
				if _, err := io.ReadFull(conn, body); err != nil {
					return
				}
				if _, err := conn.Write(append(frameHeader[:], body...)); err != nil {
					return
				}
				io.Copy(io.Discard, conn)
			}()
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	return address.IP.String(), address.Port
}

// One session's setup: a resident transport, one resident forward, and two
// outbound exchange connections, a transport's and a forward's, each through
// its handshake. Reports the bytes the setup allocates.
func BenchmarkConnectSessionSetupAllocation(b *testing.B) {
	host, port := startExchangeHeaderEchoForTest(b)
	settings := DefaultExchangeSettings()
	exchange := &Exchange{settings: settings}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		clientId := server.NewId()
		transport := newResidentTransport(ctx, exchange, ExchangeHeader{Op: ExchangeOpTransport}, clientId, server.NewId())
		forward := NewResidentForward(ctx, exchange, server.NewId())
		for _, op := range []ExchangeOp{ExchangeOpTransport, ExchangeOpForward} {
			connection, err := NewExchangeConnection(ctx, ExchangeHeader{ClientId: clientId, ResidentId: server.NewId(), Op: op}, host, port, nil, settings)
			if err != nil {
				b.Fatal(err)
			}
			connection.Close()
		}
		transport.Close()
		transport.Cancel()
		forward.Cancel()
	}
}

// The bounds of a session's queues: the transport's send and receive, the
// forward's send, the transport connection's send and receive, and the forward
// connection's send and its receive, which holds nothing.
func connectSessionQueueBoundsForTest(settings *ExchangeSettings) []int {
	return []int{
		settings.ExchangeBufferSize,
		settings.ExchangeBufferSize,
		settings.ForwardBufferSize,
		settings.ExchangeBufferSize,
		settings.ExchangeBufferSize,
		settings.ExchangeBufferSize,
		0,
	}
}

// A session's queues as the channels they were, made at their bounds.
func BenchmarkConnectSessionQueuesAsChannels(b *testing.B) {
	bounds := connectSessionQueueBoundsForTest(DefaultExchangeSettings())
	queues := make([]chan []byte, len(bounds))
	b.ReportAllocs()
	for b.Loop() {
		for i, bound := range bounds {
			queues[i] = make(chan []byte, bound)
		}
	}
}

// A session's queues as messageQueues, which allocate as they fill.
func BenchmarkConnectSessionQueuesAsMessageQueues(b *testing.B) {
	bounds := connectSessionQueueBoundsForTest(DefaultExchangeSettings())
	queues := make([]*messageQueue, len(bounds))
	b.ReportAllocs()
	for b.Loop() {
		for i, bound := range bounds {
			queues[i] = newMessageQueue(bound)
		}
	}
}
