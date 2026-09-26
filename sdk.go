package server

import (
	"context"
	"fmt"
	"net"

	"github.com/urnetwork/connect"
)

// IPv4DialContext confines one server-owned client to IPv4 without changing
// the process-wide Connect address-family policy.
func IPv4DialContext(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4":
			return dial(ctx, "tcp4", address)
		case "udp", "udp4":
			return dial(ctx, "udp4", address)
		case "tcp6", "udp6":
			return nil, fmt.Errorf("server IPv4 client: IPv6 transport %q is not supported", network)
		default:
			return nil, fmt.Errorf("server IPv4 client: unsupported transport network %q", network)
		}
	}
}

// ForceIPv4ConnectSettings applies the server's IPv4 carrier policy to one
// Connect client instance. Preserve the original packet factory and copy the
// original settings before installing the wrapper to avoid recursive dialing.
func ForceIPv4ConnectSettings(settings *connect.ConnectSettings) {
	base := *settings
	var packetConnFactory func(context.Context) (net.PacketConn, error)
	if settings.DialContextSettings != nil {
		packetConnFactory = settings.DialContextSettings.PacketConnFactory
	}
	settings.DialContextSettings = &connect.DialContextSettings{
		DialContext:       IPv4DialContext(base.DialContext),
		PacketConnFactory: packetConnFactory,
	}
}

// CapServerTunIPv4 keeps an inner gVisor TUN below the IPv6-enabling MTU.
// A caller's smaller MTU remains intact.
func CapServerTunIPv4(settings *connect.TunSettings) {
	settings.Mtu = min(settings.Mtu, connect.DefaultMtu)
}
