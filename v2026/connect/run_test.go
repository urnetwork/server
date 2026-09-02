package connect

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/urnetwork/server/v2026"
)

func TestConnectWarmupTargetsOnlyIPDatabase(t *testing.T) {
	want := []server.WarmupTarget{server.WarmupTargetIPDatabase}
	if got := connectWarmupTargets(); !slices.Equal(got, want) {
		t.Fatalf("connect warmup targets = %v, want %v", got, want)
	}
}

func TestRunRejectsInvalidInputsBeforeEnvironmentAccess(t *testing.T) {
	if err := Run(nil, RunOptions{Port: 1}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 0}); err == nil {
		t.Fatal("zero port was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 65_536}); err == nil {
		t.Fatal("overflow port was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, TLSDefaultHostName: " 127.0.1.1"}); err == nil {
		t.Fatal("whitespace-padded TLS fallback hostname was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, TLSDefaultHostName: "127.0.1.1/path"}); err == nil {
		t.Fatal("path-shaped TLS fallback hostname was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, DirectH3LoopbackMode: true}); err == nil {
		t.Fatal("direct H3 mode without a TLS fallback hostname was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, TLSDefaultHostName: "192.0.2.1", DirectH3LoopbackMode: true}); err == nil {
		t.Fatal("direct H3 mode with a non-loopback TLS fallback hostname was accepted")
	}
}

func TestRunSettingsUseExplicitTLSDefaultHostWithoutEnablingSelfSign(t *testing.T) {
	settings := exchangeSettingsForRun(RunOptions{Port: 443, TLSDefaultHostName: "127.0.1.1"})
	transportTLS := settings.ConnectHandlerSettings.TransportTlsSettings
	if transportTLS == nil || transportTLS.DefaultHostName != "127.0.1.1" || transportTLS.EnableSelfSign {
		t.Fatalf("transport TLS settings = %+v", transportTLS)
	}
	ordinary := exchangeSettingsForRun(RunOptions{Port: 443})
	if ordinary.ConnectHandlerSettings.TransportTlsSettings.DefaultHostName != "" {
		t.Fatalf("ordinary production TLS fallback = %q", ordinary.ConnectHandlerSettings.TransportTlsSettings.DefaultHostName)
	}
	if !ordinary.ConnectHandlerSettings.EnableProxyProtocol {
		t.Fatal("ordinary production ingress lost Proxy Protocol")
	}
}

// The real simulator reaches each operator's UDP sockets without an nginx
// hop. This is the exact setting boundary that previously discarded every
// direct QUIC Initial while the HTTP status endpoint remained healthy.
func TestRunSettingsDirectH3LoopbackBypassesProxyProtocol(t *testing.T) {
	settings := exchangeSettingsForRun(RunOptions{
		Port:                 443,
		TLSDefaultHostName:   "127.0.1.1",
		DirectH3LoopbackMode: true,
	})
	if settings.ConnectHandlerSettings.EnableProxyProtocol {
		t.Fatal("direct H3 loopback ingress still requires a Proxy Protocol header")
	}
	if settings.ConnectHandlerSettings.TransportTlsSettings.DefaultHostName != "127.0.1.1" {
		t.Fatalf("direct H3 TLS fallback = %q", settings.ConnectHandlerSettings.TransportTlsSettings.DefaultHostName)
	}
}

// Reproduces the production router boundary that used to replace the
// simulator's customized handler settings with a fresh default snapshot.
func TestRunRouterRetainsDirectH3LoopbackSettings(t *testing.T) {
	settings := exchangeSettingsForRun(RunOptions{
		Port:                 443,
		TLSDefaultHostName:   "127.0.1.1",
		DirectH3LoopbackMode: true,
	})
	exchange := &Exchange{settings: settings}
	handlerSettings := connectHandlerSettingsFromExchange(exchange)
	if handlerSettings != &settings.ConnectHandlerSettings {
		t.Fatal("router did not retain the exchange's exact handler settings snapshot")
	}
	if handlerSettings.EnableProxyProtocol {
		t.Fatal("router restored Proxy Protocol on direct loopback ingress")
	}
	if handlerSettings.TransportTlsSettings.DefaultHostName != "127.0.1.1" {
		t.Fatalf("router TLS fallback = %q", handlerSettings.TransportTlsSettings.DefaultHostName)
	}
}

// Exercises the exact exchange-to-router settings handoff through a real QUIC
// Initial. Before the fix this handoff installed the Proxy Protocol wrapper,
// so the listener consumed the datagram without ever reaching TLS.
func TestRunRouterDirectH3LoopbackCompletesHandshake(t *testing.T) {
	settings := exchangeSettingsForRun(RunOptions{
		Port:                 443,
		TLSDefaultHostName:   "127.0.0.1",
		DirectH3LoopbackMode: true,
	})
	settings.ConnectHandlerSettings.TransportTlsSettings.EnableSelfSign = true
	exchange := &Exchange{settings: settings}
	handlerSettings := connectHandlerSettingsFromExchange(exchange)
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key := connectListenerKey{transport: connectListenerTransportH3, port: 0}
	ctx, cancel := context.WithCancel(context.Background())
	listenerUp := make(chan struct{})
	var listenerUpOnce sync.Once
	handler := &ConnectHandler{
		ctx:          ctx,
		settings:     handlerSettings,
		transportTls: server.NewTransportTls(map[string]bool{}, handlerSettings.TransportTlsSettings),
		listenerStates: map[connectListenerKey]bool{
			key: false,
		},
		listenerStateObserver: func(observedKey connectListenerKey, up bool) {
			if observedKey == key && up {
				listenerUpOnce.Do(func() { close(listenerUp) })
			}
		},
	}
	listenerExit := make(chan error, 1)
	go func() {
		listenerExit <- handler.listenH3(key, packetConn)
	}()
	select {
	case <-listenerUp:
	case err := <-listenerExit:
		cancel()
		t.Fatalf("H3 listener exited before readiness: %v", err)
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	connection, err := quic.DialAddr(
		dialCtx,
		packetConn.LocalAddr().String(),
		&tls.Config{
			InsecureSkipVerify: true, // deterministic self-signed test identity
			MinVersion:         tls.VersionTLS13,
			ServerName:         "127.0.0.1",
		},
		&quic.Config{HandshakeIdleTimeout: time.Second},
	)
	dialCancel()
	if err != nil {
		cancel()
		<-listenerExit
		t.Fatalf("direct H3 handshake: %v", err)
	}
	if err := connection.CloseWithError(0, "test complete"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-listenerExit; err == nil {
		t.Fatal("canceled H3 listener returned no error")
	}
}

// The explicit mode cannot be combined with an externally reachable listener;
// production's ordinary Proxy Protocol path remains valid on that address.
func TestRunDirectH3LoopbackModeRejectsNonLoopbackListener(t *testing.T) {
	options := RunOptions{Port: 443, TLSDefaultHostName: "127.0.1.1", DirectH3LoopbackMode: true}
	if err := validateRunListenIPv4(options, "192.0.2.1"); err == nil {
		t.Fatal("direct H3 loopback mode accepted an external listener")
	}
	if err := validateRunListenIPv4(options, "127.0.1.1"); err != nil {
		t.Fatalf("direct H3 loopback listener: %v", err)
	}
	options.DirectH3LoopbackMode = false
	if err := validateRunListenIPv4(options, "192.0.2.1"); err != nil {
		t.Fatalf("ordinary production listener: %v", err)
	}
}
