package connect

import (
	"context"
	"testing"
)

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
