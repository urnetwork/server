package server

import (
	"context"
	"errors"
	"net"
	"reflect"
	"syscall"
	"testing"

	"github.com/urnetwork/connect"
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

// The server keeps its standard mtu and independently refuses ipv6. Applying
// the owner policy twice changes neither the chosen mtu nor other settings.
func TestCapServerTunIPv4(t *testing.T) {
	if got := connect.DefaultTunSettings().Mtu; got != 1280 {
		t.Fatalf("default inner mtu = %d, want 1280", got)
	}
	for _, mtu := range []int{1280, 900, 1500} {
		settings := connect.DefaultTunSettings()
		settings.Mtu = mtu
		settings.Log = connect.NewNoopLogger()
		want := *settings
		want.Mtu = min(mtu, 1280)
		want.DisableIpv6 = true
		for range 2 {
			CapServerTunIPv4(settings)
		}
		tun, err := connect.CreateTun(t.Context(), settings)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tun.Close() })
		if tun.Ipv6Enabled() {
			t.Fatalf("SERVER_TUN_IPV6_ENABLED_AT_MTU1280: requested mtu=%d", mtu)
		}
		if addresses := tun.LocalAddresses(); len(addresses) != 1 || !addresses[0].Is4() {
			t.Fatalf("requested mtu %d: server addresses = %v, want one ipv4 address", mtu, addresses)
		}
		for _, network := range []string{"tcp6", "udp6"} {
			if conn, err := tun.DialContext(t.Context(), network, "[2001:db8::25]:443"); conn != nil || err != syscall.EAFNOSUPPORT {
				if conn != nil {
					_ = conn.Close()
				}
				t.Fatalf("requested mtu %d: server %s dial error = %v, want %v", mtu, network, err, syscall.EAFNOSUPPORT)
			}
		}
		if err := tun.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*settings, want) {
			t.Fatalf("requested mtu %d: server policy changed fields beyond mtu and family", mtu)
		}
	}
}
