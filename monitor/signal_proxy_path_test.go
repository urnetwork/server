package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestProxyPathSignalSyntheticProblems(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, proxyAllocationMarker):
				return "synthetic-proxy-g1-current|80:12689,8080:12719,8081:12720,8082:12721|200", nil
			case strings.Contains(command, proxyRouteMarker):
				return "networkd_start=200\nlb_start=100\nv4_routes=0\nv6_routes=1\nv4_rules=0\nv6_rules=1", nil
			case strings.Contains(command, edgeAutoUpgradeMarker):
				return "periodic_enable=1\napt-daily.timer=enabled\napt-daily-upgrade.timer=enabled\napt-daily.service=static\napt-daily-upgrade.service=static\nunattended-upgrades.service=enabled", nil
			default:
				return "", errors.New("unexpected synthetic host command")
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			if strings.Contains(strings.Join(args, " "), "https://invalid:invalid@") {
				return "000", errors.New("synthetic HTTPS proxy timeout")
			}
			return "407", nil
		},
		tcpFn: func(string, string, []byte, int) ([]byte, error) {
			return []byte{0x05, 0x00}, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "proxy-1",
		Proxy: &ProxyHostSettings{
			PublicHostname:   "proxy.example",
			PublicInterface:  "eno1",
			RoutingTable:     100,
			LoadBalancerUnit: "warp-main-lb-eno1.service",
			AddressFamilies:  []string{"ipv4"},
		},
	})

	alerts, err := NewProxyPathSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"proxy-public-handshake", "policy-route-drift", "edge-auto-upgrades"} {
		alert := requireAlertClass(t, alerts, class)
		if alert.SignalKey != "proxy-path" {
			t.Fatalf("class %s signal key = %q", class, alert.SignalKey)
		}
	}
}
