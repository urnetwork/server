package controlplane

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/urnetwork/connect"
)

func TestIPv4DialContextMapsUnspecifiedTCPToTCP4(t *testing.T) {
	wantErr := errors.New("stop after observing the network")
	gotNetwork := ""
	dialContext := ipv4DialContext(func(_ context.Context, network string, _ string) (net.Conn, error) {
		gotNetwork = network
		return nil, wantErr
	})

	_, err := dialContext(context.Background(), "tcp", "api.bringyour.com:443")
	if !errors.Is(err, wantErr) {
		t.Fatalf("dial error = %v, want injected error", err)
	}
	if gotNetwork != "tcp4" {
		t.Fatalf("underlying network = %q, want tcp4", gotNetwork)
	}
}

func TestIPv4DialContextKeepsTCP4(t *testing.T) {
	wantErr := errors.New("stop after observing the network")
	gotNetwork := ""
	dialContext := ipv4DialContext(func(_ context.Context, network string, _ string) (net.Conn, error) {
		gotNetwork = network
		return nil, wantErr
	})

	_, err := dialContext(context.Background(), "tcp4", "connect.bringyour.com:443")
	if !errors.Is(err, wantErr) {
		t.Fatalf("dial error = %v, want injected error", err)
	}
	if gotNetwork != "tcp4" {
		t.Fatalf("underlying network = %q, want tcp4", gotNetwork)
	}
}

func TestIPv4DialContextRejectsTCP6BeforeDial(t *testing.T) {
	called := false
	dialContext := ipv4DialContext(func(_ context.Context, _ string, _ string) (net.Conn, error) {
		called = true
		return nil, nil
	})

	if _, err := dialContext(context.Background(), "tcp6", "[2001:db8::1]:443"); err == nil {
		t.Fatal("explicit tcp6 dial succeeded")
	}
	if called {
		t.Fatal("explicit tcp6 request reached the underlying dialer")
	}
}

func TestIPv4DialContextMapsUnspecifiedUDPToUDP4(t *testing.T) {
	wantErr := errors.New("stop after observing the network")
	gotNetwork := ""
	dialContext := ipv4DialContext(func(_ context.Context, network string, _ string) (net.Conn, error) {
		gotNetwork = network
		return nil, wantErr
	})

	_, err := dialContext(context.Background(), "udp", "resolver.example:53")
	if !errors.Is(err, wantErr) {
		t.Fatalf("dial error = %v, want injected error", err)
	}
	if gotNetwork != "udp4" {
		t.Fatalf("underlying network = %q, want udp4", gotNetwork)
	}
}

func TestIPv4DialContextRejectsUDP6BeforeDial(t *testing.T) {
	called := false
	dialContext := ipv4DialContext(func(_ context.Context, _ string, _ string) (net.Conn, error) {
		called = true
		return nil, nil
	})

	if _, err := dialContext(context.Background(), "udp6", "[2001:db8::1]:53"); err == nil {
		t.Fatal("explicit udp6 dial succeeded")
	}
	if called {
		t.Fatal("explicit udp6 request reached the underlying dialer")
	}
}

func TestForceIPv4ConnectSettingsPreservesInjectedDialer(t *testing.T) {
	wantErr := errors.New("stop after observing the network")
	gotNetwork := ""
	settings := connect.DefaultConnectSettings()
	settings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(_ context.Context, network string, _ string) (net.Conn, error) {
			gotNetwork = network
			return nil, wantErr
		},
	}
	forceIPv4ConnectSettings(settings)

	_, err := settings.DialContext(context.Background(), "tcp", "connect.bringyour.com:443")
	if !errors.Is(err, wantErr) {
		t.Fatalf("dial error = %v, want injected error", err)
	}
	if gotNetwork != "tcp4" {
		t.Fatalf("underlying network = %q, want tcp4", gotNetwork)
	}

	gotNetwork = ""
	if _, err := settings.DialContext(context.Background(), "tcp6", "[2001:db8::1]:443"); err == nil {
		t.Fatal("IPv4-only Connect settings accepted an explicit tcp6 dial")
	}
	if gotNetwork != "" {
		t.Fatalf("explicit tcp6 request reached the underlying dialer as %q", gotNetwork)
	}
}

func TestClientStrategySettingsLeaveIndependentSettingsUntouched(t *testing.T) {
	gotNetwork := ""
	untouched := connect.DefaultClientStrategySettings()
	untouched.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(_ context.Context, network string, _ string) (net.Conn, error) {
			gotNetwork = network
			return nil, errors.New("stop after observing the network")
		},
	}

	forced := clientStrategySettings()
	if forced.ConnectSettings.DialContextSettings == nil {
		t.Fatal("Connect strategy has no IPv4-only dial boundary")
	}
	_, _ = untouched.ConnectSettings.DialContext(context.Background(), "tcp", "connect.bringyour.com:443")
	if gotNetwork != "tcp" {
		t.Fatalf("strategy construction changed an independent dial network to %q", gotNetwork)
	}
}
