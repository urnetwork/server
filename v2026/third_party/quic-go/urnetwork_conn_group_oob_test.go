//go:build darwin || linux || freebsd

package quic

// urnetwork: a ConnGroup's sockets keep quic-go's oob path (URNETWORK.md, "One
// transport over a group of sockets").

import (
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quic-go/quic-go/internal/protocol"
)

// Every socket of a group is read through quic-go's oob conn, as a lone socket
// handed to a Transport is: each datagram arrives with its ECN bits, whichever
// socket it reached, and the group's capabilities (the DF bit, GSO, ECN) are a
// lone socket's.
func TestUrnetworkConnGroupReadsEverySocketWithItsOobPath(t *testing.T) {
	sockets := []*net.UDPConn{connGroupTestSocket(t), connGroupTestSocket(t)}
	group := NewConnGroup(sockets[0].LocalAddr())
	defer group.Close()
	for _, socket := range sockets {
		member, err := group.Add(socket)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := member.raw.(*oobConn); !ok {
			t.Fatalf("a socket of the group is read as a %T", member.raw)
		}
	}
	lone, err := wrapConn(connGroupTestSocket(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := group.capabilities(), lone.capabilities(); got != want {
		t.Fatalf("the group's capabilities are %+v, a lone socket's %+v", got, want)
	}

	for i, socket := range sockets {
		sender, err := net.DialUDP("udp4", nil, socket.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer sender.Close()
		rawConn, err := sender.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var setErr error
		if err := rawConn.Control(func(fd uintptr) {
			// ECT(0)
			setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TOS, 2)
		}); err != nil {
			t.Fatal(err)
		}
		if setErr != nil {
			t.Fatal(setErr)
		}
		what := fmt.Sprintf("to socket %d", i)
		if _, err := sender.Write([]byte(what)); err != nil {
			t.Fatal(err)
		}
		group.SetReadDeadline(time.Now().Add(connGroupTestTimeout))
		p, err := group.ReadPacket()
		if err != nil {
			t.Fatal(err)
		}
		if string(p.data) != what || p.remoteAddr.String() != sender.LocalAddr().String() || p.ecn != protocol.ECT0 {
			t.Fatalf("read %q from %s with ecn %v, want %q from %s with ECT(0)", p.data, p.remoteAddr, p.ecn, what, sender.LocalAddr())
		}
		p.buffer.Release()
	}
}
