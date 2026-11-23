// URNETWORK (URNETWORK.md): UConn.Write releases its interlock with Close
// after the compare-and-swap loop, as Conn.Write does, so a write allocates no
// defer record, and the interlock is unchanged.
package tls

import (
	"errors"
	"net"
	"testing"
)

// Writes to a connection this discards, so a count of allocations holds the
// write alone.
type urnetworkDiscardConn struct {
	net.Conn
}

func (urnetworkDiscardConn) Write(b []byte) (int, error) {
	return len(b), nil
}

// A client UConn after a full handshake with a local server.
func urnetworkHandshakenUConn(t *testing.T) *UConn {
	t.Helper()
	clientConn, serverConn := localPipe(t)
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- Server(serverConn, testConfig.Clone()).Handshake()
	}()
	client := UClient(clientConn, testConfig.Clone(), HelloGolang)
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return client
}

func TestUrnetworkUConnWriteAllocatesNothing(t *testing.T) {
	client := urnetworkHandshakenUConn(t)
	client.conn = urnetworkDiscardConn{Conn: client.conn}
	message := []byte("a small write")
	allocs := testing.AllocsPerRun(100, func() {
		if n, err := client.Write(message); err != nil || n != len(message) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("a write allocates %v times", allocs)
	}
}

// A write leaves the interlock as it found it, and a write after Close is
// refused with net.ErrClosed.
func TestUrnetworkUConnWriteReleasesItsInterlock(t *testing.T) {
	client := urnetworkHandshakenUConn(t)
	message := []byte("a small write")
	before := client.activeCall.Load()
	if _, err := client.Write(message); err != nil {
		t.Fatal(err)
	}
	if after := client.activeCall.Load(); after != before {
		t.Fatalf("activeCall %d after a write, %d before", after, before)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(message); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a write after Close = %v, want net.ErrClosed", err)
	}
}
