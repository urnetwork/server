//go:build unix

// Socket option readback verifies configuration, not a universal detection
// bound: TCP suppresses idle probes when application data is outstanding.
package connect

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Idle probing starts after the expected heartbeat. These values describe an
// idle probe schedule, not a guarantee about an actively writing connection.
func TestExchangeKeepAliveStartsAfterTheNormalPingCadence(t *testing.T) {
	settings := DefaultExchangeSettings()
	keepAlive := exchangeKeepAliveConfig(settings)
	if !keepAlive.Enable {
		t.Fatal("exchange keepalive is off")
	}
	if keepAlive.Idle <= settings.ExchangePingTimeout {
		t.Fatalf("keepalive idle %s does not outlast the ping every %s, so live connections probe", keepAlive.Idle, settings.ExchangePingTimeout)
	}
	if keepAlive.Idle != 11*time.Second || keepAlive.Interval != time.Second || keepAlive.Count != 4 {
		t.Fatalf("default exchange probe settings changed: %+v", keepAlive)
	}
}

// A custom ping cadence and probe settings reach the same configuration used
// by both accepted and dialed sockets.
func TestExchangeKeepAliveUsesOwnerSettings(t *testing.T) {
	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = 7 * time.Second
	settings.ExchangeKeepAliveIdleMargin = 2 * time.Second
	settings.ExchangeKeepAliveInterval = 3 * time.Second
	settings.ExchangeKeepAliveCount = 5
	want := net.KeepAliveConfig{Enable: true, Idle: 9 * time.Second, Interval: 3 * time.Second, Count: 5}
	if got := exchangeKeepAliveConfig(settings); got != want {
		t.Fatalf("exchange probe settings = %+v, want %+v", got, want)
	}
}

// Dialed sockets retain the configured probe interval and count.
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

	keepAlive := exchangeKeepAliveConfig(DefaultExchangeSettings())
	configureExchangeTcpConn(tcpConn, keepAlive)
	if !tcpKeepAliveEnabled(t, tcpConn) {
		t.Fatal("dialed exchange socket has no TCP keepalive")
	}
	assertTcpKeepAliveProbes(t, tcpConn, keepAlive)
}

// Accepted sockets inherit the same configuration from their listener.
func TestExchangeListenerAcceptsWithTcpKeepAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keepAlive := exchangeKeepAliveConfig(DefaultExchangeSettings())
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

// Reads the kernel option from the actual connection under test.
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

// Reads probe options without inferring how active data will affect probing.
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
