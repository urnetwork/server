// This file validates the certificate-pin snapshot shared by one fleet pass.
package fleetprobe

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/urnetwork/operator-proxy/ingest"
)

// Converts the server's served pin set into the tunnel's pin
// map, or refuses it.
//
// What pins are for. Every request a probe makes rides the tunnel of the
// provider being measured, so the provider is on the path of every TLS
// handshake. Ordinary WebPKI verification already means it cannot answer for
// a host without a certificate a public CA issued for that host; a pin
// narrows that to the keys the server itself observed for the host on a
// direct connection, so even a mis-issued, chain-valid certificate from
// another issuer is refused (providertunnel.checkPin). The prober used to
// require a pin for its three geolocation sources, whose forged answer would
// have been a forged location; those sources are gone (GEOMAP D24), and no
// host is required to carry one now.
//
// How a host without a pin is verified -- which is every pooled destination
// the server does not observe, and by default the operator's own /ip echo:
// by ordinary WebPKI chain and hostname verification, exactly as any https
// client does (MinVersion TLS 1.2, ServerName set, InsecureSkipVerify never
// set). That is the bar the egress-health destinations have always been held
// to, for the reasons on providertunnel.Tunnel.HttpClientForHosts: forging a
// pass takes a mis-issued certificate, which being on the path does not give
// a provider, and pinning a hundred-odd destinations would turn every routine
// leaf rotation into a provider falsely charged with a failed site. A host
// the server does serve a pin for -- a pooled destination, or the echo host,
// which is the one host whose answer now places the provider and the first
// the server should observe -- is pinned, and keeps its pin through the
// allowlist (HttpClientForHosts never unpins).
//
// The contract kept from before:
//
//   - half a pin is not a pin: a served host whose leaf or intermediate is
//     empty makes the whole set an error, because the server never writes
//     one (its observation job errors rather than store a chain it could not
//     take an issuer from), so the shape means something is wrong upstream;
//   - the served set cannot widen the tunnel's allowlist: the pin map is also
//     the allowlist of pinned hosts, so before a tunnel is opened the set is
//     cut down to the hosts that probe dials (restrictPins), and a pin for any
//     other host never reaches it.
//
// An empty served set is valid and yields an empty map.
func ValidatePins(servedPins map[string]ingest.GeolocationPin) (map[string][]string, error) {
	pins := make(map[string][]string, len(servedPins))
	var halfPinned []string
	for host, pin := range servedPins {
		if pin.Leaf == "" || pin.Intermediate == "" {
			halfPinned = append(halfPinned, host)
			continue
		}
		pins[host] = []string{pin.Leaf, pin.Intermediate}
	}
	if 0 < len(halfPinned) {
		sort.Strings(halfPinned)
		return nil, fmt.Errorf(
			"the server served an incomplete certificate pin (no leaf or no intermediate) for %s. The server never stores one, so its pin set is broken; the prober will not run on part of it",
			strings.Join(halfPinned, " "))
	}
	return pins, nil
}

// Keeps the pins for the hosts one probe dials and drops the
// rest, so a set fetched over the network can never widen the tunnel's
// allowlist, which a pinned host is part of (see ValidatePins). Dropping is
// the ordinary case, not a fault -- the server's observation job still covers
// the retired geolocation sources -- so it is silent. The result is a new map;
// the caller's is not touched.
func restrictPins(pins map[string][]string, hosts []string) map[string][]string {
	dialed := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		dialed[normalizePinHost(host)] = true
	}
	restricted := make(map[string][]string, len(hosts))
	for host, allowed := range pins {
		if dialed[normalizePinHost(host)] {
			restricted[host] = append([]string(nil), allowed...)
		}
	}
	return restricted
}

// The host a pin-map key or a dialed host names, the way
// providertunnel compares them: lower-case, with any stray :port dropped.
func normalizePinHost(host string) string {
	if bare, _, err := net.SplitHostPort(host); err == nil {
		host = bare
	}
	return strings.ToLower(host)
}
