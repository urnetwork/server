package server

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestForceIPv4ConnectSettingsPreservesInstanceSeams(t *testing.T) {
	stop := errors.New("synthetic dial")
	called := ""
	packetFactoryCalled := false
	packetFactory := func(context.Context) (net.PacketConn, error) {
		packetFactoryCalled = true
		return nil, stop
	}
	settings := connect.DefaultConnectSettings()
	settings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(_ context.Context, network, _ string) (net.Conn, error) {
			called = network
			return nil, stop
		},
		PacketConnFactory: packetFactory,
	}
	ForceIPv4ConnectSettings(settings)
	if _, err := settings.DialContext(context.Background(), "tcp", "example.invalid:443"); !errors.Is(err, stop) || called != "tcp4" {
		t.Fatalf("IPv4 dial = (%q, %v), want tcp4 and synthetic error", called, err)
	}
	called = ""
	if _, err := settings.DialContext(context.Background(), "tcp6", "[2001:db8::1]:443"); err == nil || called != "" {
		t.Fatalf("IPv6 dial reached underlying dialer: (%q, %v)", called, err)
	}
	if _, err := settings.DialContextSettings.PacketConnFactory(context.Background()); !errors.Is(err, stop) || !packetFactoryCalled {
		t.Fatalf("packet factory was not preserved: called=%v err=%v", packetFactoryCalled, err)
	}
}

func TestCapServerTunIPv4(t *testing.T) {
	settings := connect.DefaultTunSettings()
	CapServerTunIPv4(settings)
	if settings.Mtu != connect.DefaultMtu {
		t.Fatalf("default inner MTU = %d, want %d", settings.Mtu, connect.DefaultMtu)
	}
	settings.Mtu = 900
	CapServerTunIPv4(settings)
	if settings.Mtu != 900 {
		t.Fatalf("lower custom MTU = %d, want 900", settings.Mtu)
	}
}
