// The allocation of one client session's setup in connect (LBALTOPT2.md
// §3.5, R23): its resident transport, one resident forward, and the outbound
// exchange connection of each, dialed over loopback through the real header
// handshake. Sessions last a median 4.4 s in production, so this is paid at
// the session rate. The queues are sized for their peak, ExchangeBufferSize
// and ForwardBufferSize messages each, allocated in full at construction.
package connect

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/urnetwork/server"
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
// its handshake. Reports the bytes the setup allocates, and the share of the
// peak-sized queues.
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
	// the peak-sized queues: the transport's two, the forward's one, and the
	// transport connection's two and the forward connection's send queue,
	// each a slice header a slot
	queueByteCount := (5*settings.ExchangeBufferSize + settings.ForwardBufferSize) * 24
	b.ReportMetric(float64(queueByteCount), "queue-B/session")
}
