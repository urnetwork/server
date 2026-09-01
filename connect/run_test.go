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
}
