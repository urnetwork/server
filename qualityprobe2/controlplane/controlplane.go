// Package controlplane builds the direct clients used to reach the operator's
// API and Connect services. Probe destinations use a separate, tunnel-only
// client and must never use anything from this package.
package controlplane

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/urnetwork/connect"
)

// DialContext is the part of net.Dialer used by the deterministic tests and
// by the HTTP transport below.
type DialContext func(context.Context, string, string) (net.Conn, error)

// ipv4DialContext maps an unspecified stream or packet dial to its IPv4
// network and rejects an explicit IPv6 request. Control-plane calls must
// follow the same IPv4-only policy as the hosted proxy; silently accepting
// IPv6 here would make the two paths disagree again.
func ipv4DialContext(dialContext DialContext) DialContext {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4":
			return dialContext(ctx, "tcp4", address)
		case "udp", "udp4":
			return dialContext(ctx, "udp4", address)
		case "tcp6", "udp6":
			return nil, fmt.Errorf("controlplane: ipv6 dial refused for %s", address)
		default:
			return nil, fmt.Errorf("controlplane: unsupported network %q for %s", network, address)
		}
	}
}

// NewHTTPClient returns an IPv4-only client for direct API calls. The default
// transport is cloned so proxy, TLS, pooling, and HTTP/2 behavior stay aligned
// with net/http while its dial boundary is made explicit.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	netDialer := &net.Dialer{}
	transport.DialContext = ipv4DialContext(netDialer.DialContext)
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// forceIPv4ConnectSettings installs an IPv4-only dial boundary on one Connect
// strategy. Keep the original settings in a value copy: a method value on the
// pointer would observe the DialContextSettings installed below and recurse
// back into itself.
//
// Connect's developer family policy is process-wide. Using it here would also
// alter unrelated control clients in a taskworker process, while this package
// owns only the provider-tunnel strategy.
func forceIPv4ConnectSettings(settings *connect.ConnectSettings) {
	base := *settings
	settings.DialContextSettings = &connect.DialContextSettings{
		DialContext: ipv4DialContext(base.DialContext),
	}
}

// clientStrategySettings returns the normal Connect strategy with an
// IPv4-only dial boundary. This covers both API requests and the Connect
// websocket used to build a provider tunnel; the data-plane TUN remains
// dual-stack and separate.
func clientStrategySettings() *connect.ClientStrategySettings {
	settings := connect.DefaultClientStrategySettings()
	forceIPv4ConnectSettings(&settings.ConnectSettings)
	return settings
}

// NewClientStrategy returns the IPv4-only strategy used by provider tunnels.
func NewClientStrategy(ctx context.Context) *connect.ClientStrategy {
	return connect.NewClientStrategy(ctx, clientStrategySettings())
}
