//go:build linux

package quic

// urnetwork: a ConnGroup's GSO batches reach the wire (URNETWORK.md, "One
// transport over a group of sockets").

import (
	"bytes"
	"testing"

	"github.com/quic-go/quic-go/internal/protocol"
)

// A GSO batch written through the group leaves as segments of the asked size,
// through one socket of the group, where the kernel supports GSO.
func TestUrnetworkConnGroupSendsAGsoBatchThroughItsSocket(t *testing.T) {
	socket := connGroupTestSocket(t)
	group := NewConnGroup(socket.LocalAddr())
	defer group.Close()
	if _, err := group.Add(socket); err != nil {
		t.Fatal(err)
	}
	if !group.capabilities().GSO {
		t.Skip("no GSO on this kernel")
	}
	peer := connGroupTestSocket(t)
	segments := [][]byte{bytes.Repeat([]byte{1}, 1000), bytes.Repeat([]byte{2}, 1000)}
	batch := append(append([]byte{}, segments[0]...), segments[1]...)
	if n, err := group.WritePacket(batch, peer.LocalAddr(), nil, 1000, protocol.ECNUnsupported); err != nil || n != len(batch) {
		t.Fatalf("the batch: %d, %v", n, err)
	}
	for _, segment := range segments {
		if source := connGroupTestReadFrom(t, peer, segment); source.String() != socket.LocalAddr().String() {
			t.Fatalf("a segment left through %s", source)
		}
	}
}
