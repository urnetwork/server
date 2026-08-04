package connect

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type connectHandlerFailingSyscallPacketConn struct {
	net.PacketConn
}

func (self *connectHandlerFailingSyscallPacketConn) SyscallConn() (syscall.RawConn, error) {
	return nil, errors.New("synthetic syscall conn failure")
}

func newLifecycleTestConnectHandler() *ConnectHandler {
	ctx, cancel := context.WithCancel(context.Background())
	activeZero := make(chan struct{})
	close(activeZero)
	return &ConnectHandler{
		ctx:        ctx,
		cancel:     cancel,
		activeZero: activeZero,
	}
}

func TestConnectHandlerWaitForIdleBlocksUntilHandlerReturns(t *testing.T) {
	handler := newLifecycleTestConnectHandler()
	defer handler.Close()

	if !handler.beginHandle() {
		t.Fatal("open handler rejected")
	}
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer blockedCancel()
	if handler.WaitForIdle(blockedCtx) {
		t.Fatal("WaitForIdle returned while a handler was active")
	}

	handler.endHandle()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	defer readyCancel()
	if !handler.WaitForIdle(readyCtx) {
		t.Fatal("WaitForIdle did not observe the handler return")
	}
}

func TestConnectHandlerCloseRejectsNewHandlers(t *testing.T) {
	handler := newLifecycleTestConnectHandler()
	handler.Close()

	if handler.beginHandle() {
		handler.endHandle()
		t.Fatal("closed handler accepted new work")
	}
}

func TestConnectHandlerStartHandleReservesAdmissionBeforeSpawn(t *testing.T) {
	handler := newLifecycleTestConnectHandler()
	started := make(chan struct{})
	release := make(chan struct{})
	if !handler.startHandle(func() {
		close(started)
		<-release
	}) {
		t.Fatal("open handler rejected")
	}
	<-started

	handler.Close()
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer blockedCancel()
	if handler.WaitForIdle(blockedCtx) {
		t.Fatal("WaitForIdle missed a synchronously admitted worker")
	}

	close(release)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	defer readyCancel()
	if !handler.WaitForIdle(readyCtx) {
		t.Fatal("WaitForIdle did not observe the admitted worker return")
	}
}

func TestConnectHandlerZeroPortIsDisabled(t *testing.T) {
	if connectHandlerPortEnabled(0) {
		t.Fatal("zero port must disable the listener")
	}
}

func TestConnectHandlerNegativePortIsDisabled(t *testing.T) {
	if connectHandlerPortEnabled(-1) {
		t.Fatal("negative port must disable the listener")
	}
}

func TestConnectHandlerPreboundPacketConnEnablesZeroPort(t *testing.T) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()

	if !connectHandlerPacketEndpointEnabled(0, packetConn) {
		t.Fatal("prebound packet conn must enable a zero-port endpoint")
	}
}

func TestConnectHandlerCloseJoinsAndClosesPreboundPacketConns(t *testing.T) {
	h3PacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h3Address := h3PacketConn.LocalAddr().String()

	dnsPacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		h3PacketConn.Close()
		t.Fatal(err)
	}
	dnsAddress := dnsPacketConn.LocalAddr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exchange := &Exchange{
		settings: DefaultExchangeSettings(),
	}
	settings := DefaultConnectHandlerSettings()
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	handler := NewConnectHandlerWithPacketConns(
		ctx,
		server.NewId(),
		exchange,
		settings,
		ConnectHandlerPacketConns{
			H3:  h3PacketConn,
			Dns: dnsPacketConn,
		},
	)

	handler.Close()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if !handler.WaitForIdle(closeCtx) {
		t.Fatal("prebound packet listeners did not stop during close")
	}

	reboundH3, err := net.ListenPacket("udp4", h3Address)
	if err != nil {
		t.Fatalf("h3 packet conn remained open after WaitForIdle: %v", err)
	}
	reboundH3.Close()

	reboundDns, err := net.ListenPacket("udp4", dnsAddress)
	if err != nil {
		t.Fatalf("dns packet conn remained open after WaitForIdle: %v", err)
	}
	reboundDns.Close()
}

func TestConnectHandlerQuicInitializationErrorClosesPreboundPacketConn(t *testing.T) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := packetConn.LocalAddr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultConnectHandlerSettings()
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	handler := &ConnectHandler{
		ctx:          ctx,
		settings:     settings,
		transportTls: server.NewTransportTls(map[string]bool{}, settings.TransportTlsSettings),
		h3PacketConn: &connectHandlerFailingSyscallPacketConn{PacketConn: packetConn},
	}

	handler.runH3()

	rebound, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatalf("failed QUIC initialization retained its packet conn: %v", err)
	}
	rebound.Close()
}
