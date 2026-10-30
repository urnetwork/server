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

// Both ends of an exchange connection carry TCP keepalive tuned past the
// exchange ping (exchangeKeepAliveConfig): quiet while the connection is live,
// and noticing a dead peer within the former keepalive's 16 s, well before the
// read deadline would.
func TestExchangeKeepAliveIsQuietWhileLiveAndBoundsADeadPeer(t *testing.T) {
	settings := DefaultExchangeSettings()
	keepAlive := exchangeKeepAliveConfig(settings.ExchangePingTimeout)
	if !keepAlive.Enable {
		t.Fatal("exchange keepalive is off")
	}
	if keepAlive.Idle <= settings.ExchangePingTimeout {
		t.Fatalf("keepalive idle %s does not outlast the ping every %s, so live connections probe", keepAlive.Idle, settings.ExchangePingTimeout)
	}
	deadPeer := keepAlive.Idle + time.Duration(keepAlive.Count)*keepAlive.Interval
	// the former exchange keepalive: 1 s idle, then 15 probes 1 s apart
	if formerDeadPeer := 16 * time.Second; formerDeadPeer < deadPeer {
		t.Fatalf("a dead peer is noticed %s after its last packet, later than the former %s", deadPeer, formerDeadPeer)
	}
	if settings.ExchangeReadTimeout <= deadPeer {
		t.Fatalf("keepalive notices a dead peer at %s, no sooner than the %s read deadline", deadPeer, settings.ExchangeReadTimeout)
	}
}

func TestExchangeDialedSocketKeepsTcpKeepAlive(t *testing.T) {
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

	keepAlive := exchangeKeepAliveConfig(DefaultExchangeSettings().ExchangePingTimeout)
	configureExchangeTcpConn(tcpConn, keepAlive)
	if !tcpKeepAliveEnabled(t, tcpConn) {
		t.Fatal("dialed exchange socket has no TCP keepalive")
	}
	assertTcpKeepAliveProbes(t, tcpConn, keepAlive)
}

func TestExchangeListenerAcceptsWithTcpKeepAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keepAlive := exchangeKeepAliveConfig(DefaultExchangeSettings().ExchangePingTimeout)
	listenConfig := exchangeListenConfig(keepAlive)
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

	if !tcpKeepAliveEnabled(t, serverConn.(*net.TCPConn)) {
		t.Fatal("accepted exchange socket has no TCP keepalive")
	}
	assertTcpKeepAliveProbes(t, serverConn.(*net.TCPConn), keepAlive)
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

// The socket's probe interval and count, which with the idle bound how late a
// dead peer is noticed.
func assertTcpKeepAliveProbes(t *testing.T, tcpConn *net.TCPConn, keepAlive net.KeepAliveConfig) {
	t.Helper()
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var interval, count int
	var intervalErr, countErr error
	if err := rawConn.Control(func(fd uintptr) {
		interval, intervalErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL)
		count, countErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT)
	}); err != nil {
		t.Fatal(err)
	}
	if intervalErr != nil || countErr != nil {
		t.Fatal(intervalErr, countErr)
	}
	if time.Duration(interval)*time.Second != keepAlive.Interval || count != keepAlive.Count {
		t.Fatalf("socket probes every %ds, %d times; expected %s, %d times", interval, count, keepAlive.Interval, keepAlive.Count)
	}
}
