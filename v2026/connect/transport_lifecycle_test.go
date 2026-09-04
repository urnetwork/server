package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
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

func TestConnectHandlerTlsInitializationFailurePreventsListenerStart(t *testing.T) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()

	sentinel := errors.New("synthetic TLS loader failure")
	settings := DefaultConnectHandlerSettings()
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.transportTlsLoader = func(*server.TransportTlsSettings) (*server.TransportTls, error) {
		return nil, sentinel
	}
	handler, err := newConnectHandlerWithPacketConns(
		context.Background(),
		server.NewId(),
		&Exchange{settings: DefaultExchangeSettings()},
		settings,
		ConnectHandlerPacketConns{H3: packetConn},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("constructor error = %v, want TLS loader failure", err)
	}
	if handler != nil {
		handler.Close()
		t.Fatal("TLS loader failure returned a handler capable of starting listeners")
	}
}

func TestConnectHandlerTlsInitializationSuccessReachesListenerReadiness(t *testing.T) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	settings := DefaultConnectHandlerSettings()
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
		return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
	}
	handler, err := newConnectHandlerWithPacketConns(
		context.Background(),
		server.NewId(),
		&Exchange{settings: DefaultExchangeSettings()},
		settings,
		ConnectHandlerPacketConns{H3: packetConn},
	)
	if err != nil {
		packetConn.Close()
		t.Fatal(err)
	}
	defer func() {
		handler.Close()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if !handler.WaitForIdle(closeCtx) {
			t.Error("successful TLS handler did not stop")
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := handler.ListenerReady(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("successful TLS loader never reached listener readiness: %v", handler.ListenerReady())
		}
		time.Sleep(time.Millisecond)
	}
}

// The simulator pins H3DnsPump to the same loopback ingress as H3Dns. Prove
// that exact UDP/53 carrier reaches the listener and completes TLS; a rendered
// carrier is not evidence of coverage if every handshake fails before auth.
func TestConnectHandlerLocalDNSPumpCompletesTLSHandshake(t *testing.T) {
	serverPacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	translationCtx, translationCancel := context.WithCancel(context.Background())
	defer translationCancel()
	translationSettings := clientconnect.DefaultPacketTranslationSettings()
	translationSettings.DnsTlds = [][]byte{[]byte("ur.xyz.")}
	translatedServer, err := clientconnect.NewPacketTranslation(
		translationCtx,
		clientconnect.PacketTranslationModeDecode53,
		serverPacketConn,
		translationSettings,
	)
	if err != nil {
		serverPacketConn.Close()
		t.Fatal(err)
	}
	defer translatedServer.Close()

	tlsSettings := server.DefaultTransportTlsSettings()
	tlsSettings.EnableSelfSign = true
	tlsSettings.DefaultHostName = "127.0.0.1"
	transportTLS := server.NewTransportTls(map[string]bool{}, tlsSettings)
	serverQUICTransport := &quic.Transport{Conn: translatedServer}
	defer serverQUICTransport.Close()
	listener, err := serverQUICTransport.ListenEarly(
		&tls.Config{GetConfigForClient: transportTLS.GetTlsConfigForClient},
		&quic.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	type acceptResult struct {
		conn *quic.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, acceptErr := listener.Accept(testCtx)
		accepted <- acceptResult{conn: conn, err: acceptErr}
	}()

	clientPacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	translatedClient, err := clientconnect.NewPacketTranslation(
		translationCtx,
		clientconnect.PacketTranslationModeDnsPump,
		clientPacketConn,
		translationSettings,
	)
	if err != nil {
		clientPacketConn.Close()
		t.Fatal(err)
	}
	defer translatedClient.Close()

	clientQUICTransport := &quic.Transport{Conn: translatedClient}
	defer clientQUICTransport.Close()
	clientConn, err := clientQUICTransport.DialEarly(
		testCtx,
		serverPacketConn.LocalAddr(),
		&tls.Config{
			InsecureSkipVerify: true, // The deterministic listener uses a fresh self-signed certificate.
			ServerName:         "127.0.0.1",
		},
		&quic.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.CloseWithError(0, "")
	waitForHandshake := func(role string, conn *quic.Conn) {
		handshakeComplete := conn.HandshakeComplete()
		select {
		case <-handshakeComplete:
		case <-conn.Context().Done():
			select {
			case <-handshakeComplete:
				return
			default:
			}
			t.Fatalf("%s local DNS-pump TLS handshake failed: %v", role, context.Cause(conn.Context()))
		case <-testCtx.Done():
			t.Fatalf("%s local DNS-pump TLS handshake did not complete: %v", role, testCtx.Err())
		}
	}
	waitForHandshake("client", clientConn)

	var serverConn *quic.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		serverConn = result.conn
	case <-testCtx.Done():
		t.Fatalf("local DNS-pump listener did not accept the connection: %v", testCtx.Err())
	}
	defer serverConn.CloseWithError(0, "")
	waitForHandshake("server", serverConn)
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

func TestConnectHandlerSupervisesListenerBindFailureUntilReady(t *testing.T) {
	blockedConn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	blockedPort := blockedConn.LocalAddr().(*net.UDPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultConnectHandlerSettings()
	settings.ListenerRestartInitialDelay = 5 * time.Millisecond
	settings.ListenerRestartMaxDelay = 20 * time.Millisecond
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	key := connectListenerKey{transport: connectListenerTransportH3, port: blockedPort}
	handler := &ConnectHandler{
		ctx:            ctx,
		cancel:         cancel,
		settings:       settings,
		transportTls:   server.NewTransportTls(map[string]bool{}, settings.TransportTlsSettings),
		listenerStates: map[connectListenerKey]bool{key: false},
	}
	done := make(chan struct{})
	go func() {
		handler.superviseQuicListener(connectPacketEndpoint{key: key})
		close(done)
	}()

	// The reserved port forces at least one bind failure. Releasing it lets a
	// supervised retry recover without replacing the process.
	time.Sleep(30 * time.Millisecond)
	if err := blockedConn.Close(); err != nil {
		t.Fatal(err)
	}
	readyDeadline := time.Now().Add(3 * time.Second)
	for handler.ListenerReady() != nil && time.Now().Before(readyDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := handler.ListenerReady(); err != nil {
		t.Fatalf("listener did not recover after bind became available: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("listener supervisor did not stop with its context")
	}
	if handler.ListenerReady() == nil {
		t.Fatal("canceled listener remained ready")
	}
}

func TestConnectHandlerListenerReadyUdpPortsAreExplicitAndSorted(t *testing.T) {
	handler := &ConnectHandler{
		listenerStates: map[connectListenerKey]bool{
			{transport: connectListenerTransportH3Dns, port: 8053}: true,
			{transport: connectListenerTransportH3, port: 443}:     true,
			{transport: connectListenerTransportH3Dns, port: 4053}: true,
		},
	}
	ports, err := handler.ListenerReadyUdpPorts()
	if err != nil {
		t.Fatal(err)
	}
	want := []int{443, 4053, 8053}
	if len(ports) != len(want) {
		t.Fatalf("listener ports=%v want=%v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("listener ports=%v want=%v", ports, want)
		}
	}

	handler.listenerStates[connectListenerKey{transport: connectListenerTransportH3Dns, port: 4053}] = false
	if ports, err := handler.ListenerReadyUdpPorts(); err == nil || ports != nil {
		t.Fatalf("down allocation reported ready ports=%v err=%v", ports, err)
	}
}

// Verifies one production transport finisher retains idle until a locally
// dequeued or pending pooled message has been returned.
func testConnectHandlerWorkersJoinPooledOwnership(
	t *testing.T,
	description string,
	finish func(workers *connectHandlerWorkers, stop func()),
) {
	t.Helper()
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	message := clientconnect.MessagePoolGet(2 * 1024)
	witnessBeforeRelease := clientconnect.MessagePoolShareReadOnly(message)
	witnessAfterJoin := clientconnect.MessagePoolShareReadOnly(message)
	workerEntered := make(chan struct{})
	releaseWorker := make(chan struct{})
	var workers connectHandlerWorkers
	workers.start(func() {
		pendingMessage := message
		defer clientconnect.MessagePoolReturn(pendingMessage)
		close(workerEntered)
		<-releaseWorker
	})
	select {
	case <-workerEntered:
	case <-testCtx.Done():
		close(releaseWorker)
		t.Fatalf("%s did not reach ownership barrier: %v", description, testCtx.Err())
	}

	stopEntered := make(chan struct{})
	finishDone := make(chan struct{})
	go func() {
		finish(&workers, func() {
			close(stopEntered)
		})
		close(finishDone)
	}()
	select {
	case <-stopEntered:
	case <-testCtx.Done():
		close(releaseWorker)
		t.Fatalf("%s finisher did not stop connection resources: %v", description, testCtx.Err())
	}
	select {
	case <-finishDone:
		close(releaseWorker)
		t.Fatalf("%s finisher returned before ownership release", description)
	default:
	}
	if clientconnect.MessagePoolReturn(witnessBeforeRelease) {
		close(releaseWorker)
		t.Fatalf("%s lost pooled ownership before worker release", description)
	}

	close(releaseWorker)
	select {
	case <-finishDone:
	case <-testCtx.Done():
		t.Fatalf("%s finisher did not join its workers: %v", description, testCtx.Err())
	}
	if !clientconnect.MessagePoolReturn(witnessAfterJoin) {
		t.Fatalf("%s retained pooled ownership after worker join", description)
	}
}

// H1 idle joins a writer that has dequeued a resident transport message.
func TestConnectHandlerH1WorkersJoinDequeuedMessage(t *testing.T) {
	testConnectHandlerWorkersJoinPooledOwnership(
		t,
		"h1 dequeued message",
		finishH1ConnectHandlerWorkers,
	)
}

// H3 idle joins the writer's oversized next message held between batches.
func TestConnectHandlerH3WorkersJoinPendingMessage(t *testing.T) {
	testConnectHandlerWorkersJoinPooledOwnership(
		t,
		"h3 pending message",
		finishH3ConnectHandlerWorkers,
	)
}
