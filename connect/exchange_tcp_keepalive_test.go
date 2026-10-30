//go:build unix

package connect

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Exchange liveness is the exchange's own ping against a read deadline at each
// end. Neither a dialed nor an accepted exchange socket carries TCP keepalive.
func TestExchangeDialedSocketHasNoTcpKeepAlive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := server.NewDialer(time.Second).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tcpConn := conn.(*net.TCPConn)
	if !tcpKeepAliveEnabled(t, tcpConn) {
		t.Fatal("the shared dialer no longer enables keepalive; this test no longer proves the exchange turns it off")
	}

	configureExchangeTcpConn(tcpConn)
	if tcpKeepAliveEnabled(t, tcpConn) {
		t.Fatal("dialed exchange socket keeps TCP keepalive")
	}
}

func TestExchangeListenerAcceptsWithoutTcpKeepAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listenConfig := exchangeListenConfig()
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	serverConn, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer serverConn.Close()

	if tcpKeepAliveEnabled(t, serverConn.(*net.TCPConn)) {
		t.Fatal("accepted exchange socket keeps TCP keepalive")
	}
}

func tcpKeepAliveEnabled(t *testing.T, tcpConn *net.TCPConn) bool {
	t.Helper()
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var enabled int
	var getErr error
	if err := rawConn.Control(func(fd uintptr) {
		enabled, getErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatal(getErr)
	}
	return enabled != 0
}
